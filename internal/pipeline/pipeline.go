// Package pipeline 按序编排一次完整运行：
// 采集 → 聚合 → 独立探测 → 优选 IP → 目标归并 → Diff 计划 → [DRY-RUN] 打印 + SQLite 落库。
// 全程只读 AGH（rewrite 写端点零调用，写入属第二阶段 syncer）。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/aggregate"
	"cf-opt-adguard/internal/collector"
	"cf-opt-adguard/internal/config"
	"cf-opt-adguard/internal/detector"
	"cf-opt-adguard/internal/ipselector"
	"cf-opt-adguard/internal/planner"
	"cf-opt-adguard/internal/state"
)

// Deps 依赖注入（测试用 httptest / 假 DNS 替换）。
type Deps struct {
	Client *adguard.Client
	DB     *state.DB
	HC     *http.Client     // nil = detector 默认生产客户端
	Clock  func() time.Time // nil = time.Now
	Log    *slog.Logger
	Stdout io.Writer // 计划打印输出；nil = 不打印
}

// Stats 一次运行的关键统计（cmd 汇总 / 测试断言）。
type Stats struct {
	RunID       int64
	Collected   collector.Stats
	Candidates  int
	Probed      int
	Confirmed   int
	PreferredIP string
	Plan        planner.Plan
	PrunedRows  int64 // EnforceCaps 清理行数
	Mode        string
}

// Run 执行完整流水线，返回统计与退出码（0 成功 / 2 优选 IP 为空 / 3 运行失败）。
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

	// 8) 补全 runs 审计
	kept, nc, nconf := cstats.Kept, st.Candidates, st.Confirmed
	if err := deps.DB.FinishRun(ctx, runID, state.Run{
		FinishedAt:  clock(deps),
		LogEntries:  &kept,
		Candidates:  &nc,
		Confirmed:   &nconf,
		NAdd:        plan.Count(planner.ActionAdd),
		NUpdate:     plan.Count(planner.ActionUpdate),
		NRemove:     plan.Count(planner.ActionRemove),
		PreferredIP: st.PreferredIP,
	}); err != nil {
		return fail(ExitError, fmt.Errorf("补全 runs 审计: %w", err))
	}

	// 9) 打印计划
	if deps.Stdout != nil {
		printPlan(deps.Stdout, cfg, st)
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
			Domain:     r.Host,
			CNAMEHit:   r.Signal.CNAMECF,
			CIDRHit:    r.Signal.CIDR,
			CFRayHit:   r.Signal.CFRay,
			ServerHit:  r.Signal.ServerCF,
			Score:      r.Score,
			Confidence: r.Verdict,
			Error:      r.Err,
			ProbedAt:   now,
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

// printPlan 输出人类可读的运行汇总与变更计划。
func printPlan(w io.Writer, cfg *config.Config, st Stats) {
	tag := "[DRY-RUN]"
	if st.Mode == "live" {
		tag = "[APPLY]"
	}
	fmt.Fprintf(w, "%s cf-opt-adguard 运行完成（未对 AdGuard Home 做任何写操作）\n", tag)
	fmt.Fprintf(w, "采集 %d 条（%d 页，停止: %s）｜候选 %d｜探测 %d｜confirmed %d\n",
		st.Collected.Kept, st.Collected.Pages, st.Collected.Stopped,
		st.Candidates, st.Probed, st.Confirmed)
	fmt.Fprintf(w, "优选 IP: %s（source=%s, strategy=%s）\n",
		st.PreferredIP, cfg.CFIP.Source, cfg.CFIP.Strategy)
	fmt.Fprintf(w, "变更计划: add %d / update %d / remove %d\n",
		st.Plan.Count(planner.ActionAdd), st.Plan.Count(planner.ActionUpdate),
		st.Plan.Count(planner.ActionRemove))

	printEntries := func(title string, entries []planner.Entry) {
		if len(entries) == 0 {
			return
		}
		fmt.Fprintf(w, "--- %s ---\n", title)
		for _, e := range entries {
			fmt.Fprintf(w, "  %s → %s（%s）\n", e.Domain, e.Answer, e.Reason)
		}
	}
	printEntries("add", st.Plan.Add)
	printEntries("update", st.Plan.Update)
	printEntries("remove（本阶段仅打印不执行）", st.Plan.Remove)
	if st.Plan.Empty() {
		fmt.Fprintln(w, "AGH 现状与目标一致，无需变更。")
	}
}

// clock 取注入时钟或系统时钟。
func clock(deps Deps) time.Time {
	if deps.Clock != nil {
		return deps.Clock()
	}
	return time.Now()
}

// mode 运行模式标识（本版本 cmd 层已拒绝 --apply，实际只会是 dry）。
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
