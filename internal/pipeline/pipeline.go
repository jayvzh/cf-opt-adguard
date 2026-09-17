// Package pipeline 按序编排一次完整运行：
// 采集 → 聚合 → 独立探测 → 优选 IP → 目标归并 → Diff 计划 → 打印 + SQLite 落库
// → [LIVE] syncer 执行写入 → verifier 经 AGH DNS 回查验证。
// dry 模式（默认）只读 AGH：写入阶段空转；live（--apply）由 syncer 作为唯一写侧执行。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/aggregate"
	"cf-opt-adguard/internal/collector"
	"cf-opt-adguard/internal/config"
	"cf-opt-adguard/internal/detector"
	"cf-opt-adguard/internal/ipselector"
	"cf-opt-adguard/internal/planner"
	"cf-opt-adguard/internal/state"
	"cf-opt-adguard/internal/syncer"
	"cf-opt-adguard/internal/verifier"
)

// Deps 依赖注入（测试用 httptest / 假 DNS 替换）。
type Deps struct {
	Client  *adguard.Client
	DB      *state.DB
	HC      *http.Client     // nil = detector 默认生产客户端
	Clock   func() time.Time // nil = time.Now
	Log     *slog.Logger
	Stdout  io.Writer // 计划打印输出；nil = 不打印
	DNSAddr string    // verifier 回查 DNS 地址覆盖；空 = 从 cfg.AdGuard.URL 推导（host:53）
}

// Stats 一次运行的关键统计（cmd 汇总 / 测试断言）。
type Stats struct {
	RunID       int64
	Collected   collector.Stats
	Candidates  int
	Probed      int
	Confirmed   int
	Probes      []detector.Result // 逐域探测明细（证据链，dry-run 打印用）
	PreferredIP string
	Plan        planner.Plan
	PrunedRows  int64 // EnforceCaps 清理行数
	Mode        string
	Sync        syncer.Result   // live：同步执行汇总（dry 零值）
	Verify      verifier.Result // live：回查验证汇总（dry 零值）
}

// Run 执行完整流水线，返回统计与退出码（0 成功 / 2 优选 IP 为空 / 3 运行失败 /
// 4 live 同步或验证部分失败）。
func Run(ctx context.Context, cfg *config.Config, deps Deps) (Stats, int, error) {
	var st Stats
	log := deps.Log
	now := clock(deps)
	st.Mode = mode(cfg)

	runID, err := deps.DB.InsertRun(ctx, state.Run{Mode: st.Mode, StartedAt: now})
	if err != nil {
		return st, ExitError, fmt.Errorf("写入 runs 审计: %w", err)
	}
	st.RunID = runID

	// fail：以指定退出码终止，尽力补全 runs 审计。
	fail := func(code int, err error) (Stats, int, error) {
		dbErr := deps.DB.FinishRun(ctx, runID, state.Run{
			FinishedAt: clock(deps), NFailed: 1, Note: truncate(err.Error(), 500),
		})
		if dbErr != nil {
			log.Error("补全 runs 审计失败", "err", dbErr)
		}
		return st, code, err
	}

	// 1) CF IP 段（失败降级：CIDR 信号缺失继续跑，CNAME / HTTP 头信号仍有效）
	cidrs, err := detector.LoadCFIPs(ctx, deps.HC, cachePath(cfg), []int{4, 6}, log)
	if err != nil {
		log.Warn("CF IP 段加载失败，探测将仅依赖 CNAME / HTTP 头信号", "err", err)
	}
	det := detector.New(cfg.Detector, deps.HC, log)
	if len(cidrs) > 0 {
		det.SetCIDRs(cidrs)
	}

	// 2) 采集
	col := collector.New(deps.Client, cfg.WindowDur, cfg.QueryLog, log)
	filter := newDomainFilter(cfg)
	entries, cstats, err := col.Collect(ctx, filter)
	if err != nil {
		return fail(ExitError, fmt.Errorf("采集查询日志: %w", err))
	}
	st.Collected = cstats
	log.Info("采集完成", "pages", cstats.Pages, "fetched", cstats.Fetched,
		"kept", cstats.Kept, "stopped", cstats.Stopped)

	// 3) 聚合
	cands := aggregate.Aggregate(entries, cfg.MinHits(), cfg.Aggregate.MaxDomains)
	st.Candidates = len(cands)

	// 4) 独立探测全部达阈 host
	hosts := make([]string, len(cands))
	for i, c := range cands {
		hosts[i] = c.Host
	}
	results := det.DetectAll(ctx, hosts)
	st.Probed = len(results)
	st.Probes = results
	verdicts := make(map[string]aggregate.Verdict, len(results))
	for _, r := range results {
		verdicts[r.Host] = r.Verdict
		if r.Verdict == aggregate.VerdictConfirmed {
			st.Confirmed++
		}
	}

	// 5) 优选 IP（结果为空 → 退出码 2，不生成任何 remove 计划）
	ips, err := ipselector.Select(ctx, ipselector.Options{
		Source:     cfg.CFIP.Source,
		Strategy:   cfg.CFIP.Strategy,
		IPVersions: cfg.CFIP.IPVersions,
		Resolvers:  cfg.Detector.Resolvers,
		Timeout:    cfg.Detector.Timeout,
	}, log)
	if errors.Is(err, ipselector.ErrNoIP) {
		return fail(ExitNoIP, err)
	}
	if err != nil {
		return fail(ExitError, fmt.Errorf("解析优选 IP: %w", err))
	}
	preferred := ipselector.First(ips)
	st.PreferredIP = preferred.String()

	// 6) 目标归并 + AGH 现状 + Diff（纯函数）
	targets := aggregate.ResolveTargets(cands, verdicts, aggregate.TargetsOptions{
		Mode:          cfg.Aggregate.Mode,
		AllowWildcard: cfg.Sync.Wildcard,
	})
	aghList, err := deps.Client.RewriteList(ctx)
	if err != nil {
		return fail(ExitError, fmt.Errorf("拉取 AGH 重写列表: %w", err))
	}
	managed, err := deps.DB.ListManaged(ctx)
	if err != nil {
		return fail(ExitError, fmt.Errorf("读取托管集合: %w", err))
	}
	plan := planner.Diff(targets, aghList, managed, preferred, ipVersion(preferred))
	st.Plan = plan

	// 7) 状态库落库：domains / probes / rewrites 对齐 + 容量护栏
	if err := persist(ctx, cfg, deps, now, cands, results, targets, preferred); err != nil {
		return fail(ExitError, err)
	}
	pruned, err := deps.DB.EnforceCaps(ctx)
	if err != nil {
		return fail(ExitError, fmt.Errorf("容量护栏清理: %w", err))
	}
	st.PrunedRows = pruned

	// 7.5) live：syncer 唯一写侧执行计划 → verifier 经 AGH DNS 回查验证。
	// dry 模式整段跳过（AGH 写端点与 DNS 查询零调用）。部分失败不中止：
	// 退出码 4；syncer 致命错误（认证失败 / ctx 取消）→ 运行失败退出码 3。
	if cfg.Runtime.Apply {
		syn := syncer.New(deps.Client, cfg.Sync.RateLimit, cfg.Sync.Retry, log)
		syncRes, err := syn.Sync(ctx, plan, deps.DB)
		st.Sync = syncRes
		if err != nil {
			return fail(ExitError, fmt.Errorf("同步执行: %w", err))
		}
		dnsAddr := deps.DNSAddr
		if dnsAddr == "" {
			dnsAddr = aghDNSAddr(cfg.AdGuard.URL)
		}
		ver := verifier.New(dnsAddr, cfg.Detector.Timeout, log)
		st.Verify = ver.VerifyAll(ctx, deps.DB,
			append(append([]planner.Entry{}, plan.Add...), plan.Update...))
	}

	// 8) 补全 runs 审计（NFailed = 同步失败 + 回查失败）
	kept, nc, nconf := cstats.Kept, st.Candidates, st.Confirmed
	nfailed := st.Sync.Fail + len(st.Verify.Failed)
	if err := deps.DB.FinishRun(ctx, runID, state.Run{
		FinishedAt:  clock(deps),
		LogEntries:  &kept,
		Candidates:  &nc,
		Confirmed:   &nconf,
		NAdd:        plan.Count(planner.ActionAdd),
		NUpdate:     plan.Count(planner.ActionUpdate),
		NRemove:     plan.Count(planner.ActionRemove),
		NFailed:     nfailed,
		PreferredIP: st.PreferredIP,
	}); err != nil {
		return fail(ExitError, fmt.Errorf("补全 runs 审计: %w", err))
	}

	// 9) 打印计划与执行汇总
	if deps.Stdout != nil {
		printPlan(deps.Stdout, cfg, st)
	}
	if cfg.Runtime.Apply && st.Sync.Fail+len(st.Verify.Failed) > 0 {
		return st, ExitPartial, nil
	}
	return st, ExitOK, nil
}

// persist 探测与计划产物落库：
//   - domains/probes 全量 upsert（历史与导出）；
//   - rewrites 与目标集合对齐：目标行不存在则登记 pending（active 行绝不降级），
//     陈旧 pending 行标 removed（D15：rewrites 与目标集合对齐）。
func persist(ctx context.Context, cfg *config.Config, deps Deps, now time.Time,
	cands []aggregate.Candidate, results []detector.Result,
	targets []aggregate.Target, preferred net.IP) error {

	domains := make([]state.Domain, len(cands))
	for i, c := range cands {
		domains[i] = state.Domain{
			Domain: c.Host, Zone: aggregate.ZoneOf(c.Host),
			HitCount: c.Hits, FirstSeen: now, LastSeen: now,
		}
	}
	if err := deps.DB.UpsertDomains(ctx, domains); err != nil {
		return fmt.Errorf("落库聚合域名: %w", err)
	}

	probes := make([]state.Probe, len(results))
	for i, r := range results {
		probes[i] = state.Probe{
			Domain:      r.Host,
			CNAMEHit:    r.Signal.CNAMECF,
			CIDRHit:     r.Signal.CIDR,
			CFRayHit:    r.Signal.CFRay,
			ServerHit:   r.Signal.ServerCF,
			Score:       r.Score,
			Confidence:  r.Verdict,
			CNAMEChain:  strings.Join(r.CNAMEs, ","),
			ResolvedIPs: r.IPs,
			Error:       r.Err,
			ProbedAt:    now,
		}
	}
	if err := deps.DB.UpsertProbes(ctx, probes); err != nil {
		return fmt.Errorf("落库探测结论: %w", err)
	}

	answer := preferred.String()
	ver := ipVersion(preferred)
	want := make(map[string]bool, len(targets)) // domain|answer → 本轮目标行
	for _, t := range targets {
		want[t.Domain+"|"+answer] = true
		// 目标行不存在 → 登记 pending（dry-run 已产出计划、未同步）；
		// 已存在（含 active）→ 不动，绝不降级。
		if _, err := deps.DB.GetRewrite(ctx, t.Domain, answer, ver); errors.Is(err, state.ErrNotFound) {
			rw := state.Rewrite{
				Domain: t.Domain, Answer: answer, IPVersion: ver,
				State: "pending", AGHPresent: false, UpdatedAt: now,
			}
			if err := deps.DB.UpsertRewrite(ctx, rw); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	// 陈旧 pending 行 → removed（目标集合外的历史计划行；active 行由第二阶段 syncer 管理）
	pendings, err := deps.DB.ListRewritesByState(ctx, "pending")
	if err != nil {
		return fmt.Errorf("列出 pending 行: %w", err)
	}
	for _, rw := range pendings {
		if want[rw.Domain+"|"+rw.Answer] {
			continue
		}
		if err := deps.DB.SetRewriteState(ctx, rw.Domain, rw.Answer, rw.IPVersion,
			"removed", "本次未命中目标集合"); err != nil {
			return fmt.Errorf("淘汰陈旧 pending 行 %s: %w", rw.Domain, err)
		}
	}
	return nil
}

// diagHint kept=0 且最新日志早于窗口起点时给出排查提示（AGH 记录过旧或机器时钟偏差）。
func diagHint(s collector.Stats, windowStart time.Time) string {
	if s.Newest.Before(windowStart) {
		return "（最新日志早于窗口起点：AGH 记录过旧或其机器时钟偏差，可调大 --window 或核对 AGH 时钟）"
	}
	return ""
}

// printPlan 输出人类可读的运行汇总与变更计划（dry）/ 执行结果（live）。
func printPlan(w io.Writer, cfg *config.Config, st Stats) {
	live := st.Mode == "live"
	tag := "[DRY-RUN]"
	summary := "未对 AdGuard Home 做任何写操作"
	if live {
		tag = "[APPLY]"
		summary = fmt.Sprintf("同步: 成功 %d / 失败 %d｜回查验证: 通过 %d / 失败 %d",
			st.Sync.OK, st.Sync.Fail, st.Verify.Verified, len(st.Verify.Failed))
	}
	fmt.Fprintf(w, "%s cf-opt-adguard 运行完成（%s）\n", tag, summary)
	fmt.Fprintf(w, "采集 %d 条（%d 页，停止: %s）｜候选 %d｜探测 %d｜confirmed %d\n",
		st.Collected.Kept, st.Collected.Pages, st.Collected.Stopped,
		st.Candidates, st.Probed, st.Confirmed)
	if len(st.Collected.Drops) > 0 && st.Collected.Kept < st.Collected.Fetched {
		keys := make([]string, 0, len(st.Collected.Drops))
		for k := range st.Collected.Drops {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s=%d", k, st.Collected.Drops[k])
		}
		fmt.Fprintf(w, "丢弃分布: %s\n", strings.Join(parts, "｜"))
	}
	if len(st.Probes) > 0 {
		fmt.Fprintf(w, "探测明细（域名 结论 分数/阈值 [信号] 解析 IP 或错误）:\n")
		for _, r := range st.Probes {
			var sigs []string
			if r.Signal.CNAMECF {
				sigs = append(sigs, "cname")
			}
			if r.Signal.CIDR {
				sigs = append(sigs, "cidr")
			}
			if r.Signal.CFRay {
				sigs = append(sigs, "ray")
			}
			if r.Signal.ServerCF {
				sigs = append(sigs, "srv")
			}
			if len(sigs) == 0 {
				sigs = []string{"-"}
			}
			tail := "-"
			switch {
			case r.Err != "":
				tail = "err: " + r.Err
			case len(r.IPs) > 0:
				ips := make([]string, len(r.IPs))
				for i, ip := range r.IPs {
					ips[i] = ip.String()
				}
				tail = "ip: " + strings.Join(ips, ",")
			}
			fmt.Fprintf(w, "  %-46s %-9s %d/%d [%s] %s\n",
				r.Host, string(r.Verdict), r.Score, cfg.Detector.ScoreThreshold,
				strings.Join(sigs, "+"), tail)
		}
	}
	if st.Collected.Kept == 0 && st.Collected.Fetched > 0 && !st.Collected.Newest.IsZero() {
		windowStart := time.Now().Add(-cfg.WindowDur)
		fmt.Fprintf(w, "诊断: 窗口起点=%s 最新日志=%s%s\n",
			windowStart.Format(time.RFC3339),
			st.Collected.Newest.Format(time.RFC3339),
			diagHint(st.Collected, windowStart))
	}
	fmt.Fprintf(w, "优选 IP: %s（source=%s, strategy=%s）\n",
		st.PreferredIP, cfg.CFIP.Source, cfg.CFIP.Strategy)
	planTitle := "变更计划"
	if live {
		planTitle = "执行结果"
	}
	fmt.Fprintf(w, "%s: add %d / update %d / remove %d\n",
		planTitle,
		st.Plan.Count(planner.ActionAdd), st.Plan.Count(planner.ActionUpdate),
		st.Plan.Count(planner.ActionRemove))

	syncFailed := map[string]bool{}
	for _, e := range st.Sync.Failed {
		syncFailed[e.Domain] = true
	}
	verifyFailed := map[string]bool{}
	for _, e := range st.Verify.Failed {
		verifyFailed[e.Domain] = true
	}
	removeTitle := "remove（本阶段仅打印不执行）"
	if live {
		removeTitle = "remove"
	}
	printEntries := func(title string, entries []planner.Entry) {
		if len(entries) == 0 {
			return
		}
		fmt.Fprintf(w, "--- %s ---\n", title)
		for _, e := range entries {
			if !live {
				fmt.Fprintf(w, "  %s → %s（%s）\n", e.Domain, e.Answer, e.Reason)
				continue
			}
			verb := "写入"
			if title == "remove" {
				verb = "移除"
			}
			note := verb + "成功，回查验证通过"
			switch {
			case syncFailed[e.Domain]:
				note = verb + "失败（last_error 已落库，下轮重试）"
			case verifyFailed[e.Domain]:
				note = verb + "已执行但回查未通过（AGH 应答与目标不符）"
			}
			fmt.Fprintf(w, "  %s → %s（%s）\n", e.Domain, e.Answer, note)
		}
	}
	printEntries("add", st.Plan.Add)
	printEntries("update", st.Plan.Update)
	printEntries(removeTitle, st.Plan.Remove)
	if st.Plan.Empty() {
		if live {
			fmt.Fprintln(w, "本轮无变更，AGH 保持原状。")
		} else {
			fmt.Fprintln(w, "AGH 现状与目标一致，无需变更。")
		}
	}
	if live {
		if len(st.Sync.Failed) > 0 {
			fmt.Fprintln(w, "--- 同步失败（last_error 已落库，下轮重试）---")
			for _, e := range st.Sync.Failed {
				fmt.Fprintf(w, "  %s → %s\n", e.Domain, e.Answer)
			}
		}
		if len(st.Verify.Failed) > 0 {
			fmt.Fprintln(w, "--- 回查验证失败（AGH 应答与目标不符，详见 last_error）---")
			for _, e := range st.Verify.Failed {
				fmt.Fprintf(w, "  %s → %s\n", e.Domain, e.Answer)
			}
		}
		if st.Verify.Aborted {
			fmt.Fprintln(w, "回查验证中止（上下文取消），部分条目未验证。")
		}
	}
}

// clock 取注入时钟或系统时钟。
func clock(deps Deps) time.Time {
	if deps.Clock != nil {
		return deps.Clock()
	}
	return time.Now()
}

// mode 运行模式标识：live（--apply，写入生效）或 dry（默认只读）。
func mode(cfg *config.Config) string {
	if cfg.Runtime.Apply {
		return "live"
	}
	return "dry"
}

// cachePath CF IP 段缓存路径：与状态库同目录（默认 ./data/cf_ips.json）。
func cachePath(cfg *config.Config) string {
	return filepath.Join(filepath.Dir(cfg.Runtime.DBPath), "cf_ips.json")
}

// aghDNSAddr 从 AGH 管理 URL 提取主机并拼 DNS 端口 53（verifier 回查地址）。
// URL 无法解析时按原样视为主机名。
func aghDNSAddr(aghURL string) string {
	u, err := url.Parse(aghURL)
	if err != nil || u.Hostname() == "" {
		return net.JoinHostPort(aghURL, "53")
	}
	return net.JoinHostPort(u.Hostname(), "53")
}

// ipVersion 优选 IP 的版本号。
func ipVersion(ip net.IP) int {
	if ip.To4() == nil {
		return 6
	}
	return 4
}

// truncate 错误信息截断（runs.note 列防止超长错误撑爆审计行）。
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
