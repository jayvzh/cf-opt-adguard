// Package syncer 全工程唯一允许调用 AdGuard Home rewrite 写端点的包（ARCHITECTURE §4.6）。
// 仅 live（--apply）模式由 pipeline 在 Diff 之后调用；dry 模式空转。
//
// 行为契约：
//   - 执行前重拉 rewrite list，以现状为准；
//   - 逐条执行 planner 计划：限速 + 指数退避重试（可配次数）；单条最终失败
//     记录 rewrites.last_error 并继续下一条，不中断整批；
//   - 严格幂等：add 已存在降级为 update 判断；delete 已不存在视为成功；
//   - 禁止"全量删除 + 全量添加"（只消费 planner 的增量计划）；
//   - 认证失败（401/403）立即中止整批。
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/planner"
	"cf-opt-adguard/internal/state"
)

// maxBackoff 重试退避上限。
const maxBackoff = 2 * time.Second

// Syncer 消费 planner 计划并写 AGH。并发安全（底层 Client 并发安全）。
type Syncer struct {
	client    *adguard.Client
	rateLimit time.Duration // 相邻写调用最小间隔
	retry     int           // 单条最大重试次数（不含首次）
	log       *slog.Logger
}

// New 创建同步器。rateLimit <= 0 回退默认 200ms。
func New(client *adguard.Client, rateLimit time.Duration, retry int, log *slog.Logger) *Syncer {
	if rateLimit <= 0 {
		rateLimit = 200 * time.Millisecond
	}
	if retry < 0 {
		retry = 0
	}
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{client: client, rateLimit: rateLimit, retry: retry, log: log}
}

// Result 一次同步的执行汇总。
type Result struct {
	OK     int             // 成功条数（含幂等跳过）
	Fail   int             // 最终失败条数
	Failed []planner.Entry // 最终失败条目明细（last_error 已落库）
}

// Sync 执行完整计划：先 add/update（统一为"确保 domain → answer"目标单元），
// 再 remove。返回执行汇总；error 仅致命场景（认证失败 / ctx 取消）非 nil。
func (s *Syncer) Sync(ctx context.Context, plan planner.Plan, db *state.DB) (Result, error) {
	var res Result

	list, err := s.client.RewriteList(ctx)
	if err != nil {
		return res, fmt.Errorf("同步前拉取 rewrite list: %w", err)
	}
	cur := newCurrent(list)

	// add / update 统一目标单元（同族判断 + 幂等降级，见 ensure）。
	for _, e := range append(append([]planner.Entry{}, plan.Add...), plan.Update...) {
		if err := s.ensure(ctx, db, cur, e); err != nil {
			res.Fail++
			res.Failed = append(res.Failed, e)
			if fatal(err) {
				return res, fmt.Errorf("中止同步: %w", err)
			}
			continue
		}
		res.OK++
	}

	for _, e := range plan.Remove {
		if err := s.remove(ctx, db, cur, e); err != nil {
			res.Fail++
			res.Failed = append(res.Failed, e)
			if fatal(err) {
				return res, fmt.Errorf("中止同步: %w", err)
			}
			continue
		}
		res.OK++
	}
	return res, nil
}

// fatal 认证失败或上下文取消 → 整批中止。
func fatal(err error) bool {
	return errors.Is(err, adguard.ErrAuth) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ensure 确保 domain → answer 成立（消费 add / update 计划条目）：
//   - 现状已含目标答案 → 幂等成功；
//   - 同族无现有答案 → add；
//   - 同族有现有答案 → 首条 update 为目标，其余同族条目 delete（重复清理；
//     异族答案互补共存，不触碰）。
func (s *Syncer) ensure(ctx context.Context, db *state.DB, cur *current, e planner.Entry) error {
	existing := cur.family(e.Domain, e.Answer)
	switch {
	case containsAnswer(existing, e.Answer):
		markSynced(ctx, db, s.log, e.Domain, e.Answer, e.IPVersion, time.Now())
		return nil

	case len(existing) == 0:
		return s.unit(ctx, db, e, "add", func() error {
			if err := s.client.RewriteAdd(ctx, e.Domain, e.Answer); err != nil {
				return err
			}
			cur.addAnswer(e.Domain, e.Answer)
			return nil
		}, func() {
			markSynced(ctx, db, s.log, e.Domain, e.Answer, e.IPVersion, time.Now())
		})

	default:
		first := existing[0]
		if err := s.unit(ctx, db, e, "update", func() error {
			if err := s.client.RewriteUpdate(ctx, e.Domain, first, e.Answer); err != nil {
				return err
			}
			cur.replaceAnswer(e.Domain, first, e.Answer)
			return nil
		}, func() {
			markSynced(ctx, db, s.log, e.Domain, e.Answer, e.IPVersion, time.Now())
		}); err != nil {
			return err
		}
		// 其余同族旧答案清理（cur 随删除更新，remove 阶段的同键计划条目
		// 会因现状已不存在而幂等跳过，不会重复调用 delete）。
		for _, extra := range existing[1:] {
			if err := s.unit(ctx, db, e, "delete 旧答案 "+extra, func() error {
				if err := s.client.RewriteDelete(ctx, e.Domain, extra); err != nil {
					return err
				}
				cur.removeAnswer(e.Domain, extra)
				return nil
			}, func() {
				markSynced(ctx, db, s.log, e.Domain, e.Answer, e.IPVersion, time.Now())
			}); err != nil {
				return err
			}
		}
		return nil
	}
}

// remove 执行删除计划条目；目标已不存在视为幂等成功（删除成功后状态行归档，
// ARCHITECTURE §5 状态机）。
func (s *Syncer) remove(ctx context.Context, db *state.DB, cur *current, e planner.Entry) error {
	if !cur.has(e.Domain, e.Answer) {
		markRemoved(ctx, db, s.log, e.Domain, e.Answer, e.IPVersion, time.Now())
		return nil
	}
	return s.unit(ctx, db, e, "remove", func() error {
		if err := s.client.RewriteDelete(ctx, e.Domain, e.Answer); err != nil {
			return err
		}
		cur.removeAnswer(e.Domain, e.Answer)
		return nil
	}, func() {
		markRemoved(ctx, db, s.log, e.Domain, e.Answer, e.IPVersion, time.Now())
	})
}

// unit 执行一个写单元：限速 + 指数退避重试；成功回调 onOK（状态库对齐）；
// 最终失败记录 rewrites.last_error 并返回错误（调用方计 Fail）。
func (s *Syncer) unit(ctx context.Context, db *state.DB, e planner.Entry, what string,
	fn func() error, onOK func()) error {

	if err := s.sleep(ctx, s.rateLimit); err != nil {
		return err
	}
	backoff := s.rateLimit
	var err error
	for attempt := 0; ; attempt++ {
		if err = fn(); err == nil {
			s.log.Debug("rewrite 写入成功", "op", what, "domain", e.Domain, "answer", e.Answer)
			onOK()
			return nil
		}
		if fatal(err) || attempt >= s.retry {
			break
		}
		s.log.Warn("rewrite 写入失败，准备重试", "op", what, "domain", e.Domain,
			"attempt", attempt+1, "err", err)
		if sleepErr := s.sleep(ctx, backoff); sleepErr != nil {
			return sleepErr
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
	s.markFail(ctx, db, e, fmt.Errorf("%s: %w", what, err))
	return err
}

// markFail 失败条目：保留原状态（active 仍 active——AGH 旧答案未变），只记 last_error。
func (s *Syncer) markFail(ctx context.Context, db *state.DB, e planner.Entry, err error) {
	s.log.Warn("rewrite 写入最终失败", "domain", e.Domain, "answer", e.Answer, "err", err)
	st := "pending"
	if rw, gerr := db.GetRewrite(ctx, e.Domain, e.Answer, e.IPVersion); gerr == nil {
		st = rw.State
	}
	if serr := db.SetRewriteState(ctx, e.Domain, e.Answer, e.IPVersion, st, truncate(err.Error(), 500)); serr != nil {
		s.log.Error("记录 rewrite 失败状态出错", "domain", e.Domain, "err", serr)
	}
}

// markSynced 成功条目 → active + AGHPresent=true + 刷新同步时间（新行登记 first_synced，
// 旧行 first_synced 由 UpsertRewrite 冲突分支保留）。
func markSynced(ctx context.Context, db *state.DB, log *slog.Logger, domain, answer string, ver int, now time.Time) {
	rw, err := db.GetRewrite(ctx, domain, answer, ver)
	switch {
	case errors.Is(err, state.ErrNotFound):
		rw = state.Rewrite{Domain: domain, Answer: answer, IPVersion: ver, FirstSynced: now}
	case err != nil:
		log.Error("读取 rewrite 状态行失败", "domain", domain, "err", err)
		return
	}
	rw.State, rw.AGHPresent = "active", true
	if rw.FirstSynced.IsZero() {
		rw.FirstSynced = now // 既有 pending 行首次同步成功（新行已在上方初始化）
	}
	rw.LastSynced, rw.LastError, rw.UpdatedAt = now, "", now
	if err := db.UpsertRewrite(ctx, rw); err != nil {
		log.Error("更新 rewrite 状态行失败", "domain", domain, "err", err)
	}
}

// markRemoved 删除成功条目 → removed + AGHPresent=false（归档，不清行保留审计）。
func markRemoved(ctx context.Context, db *state.DB, log *slog.Logger, domain, answer string, ver int, now time.Time) {
	rw, err := db.GetRewrite(ctx, domain, answer, ver)
	switch {
	case errors.Is(err, state.ErrNotFound):
		rw = state.Rewrite{Domain: domain, Answer: answer, IPVersion: ver}
	case err != nil:
		log.Error("读取 rewrite 状态行失败", "domain", domain, "err", err)
		return
	}
	rw.State, rw.AGHPresent = "removed", false
	rw.LastSynced, rw.LastError, rw.UpdatedAt = now, "", now
	if err := db.UpsertRewrite(ctx, rw); err != nil {
		log.Error("更新 rewrite 状态行失败", "domain", domain, "err", err)
	}
}

// sleep 上下文可取消的限速 / 退避等待。
func (s *Syncer) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// current AGH rewrite 现状的本地视图：随每次写成功同步更新，
// 保证批内后续幂等判断与真实现状一致（严格幂等的基础）。
type current map[string][]string // domain → answers

func newCurrent(list []adguard.RewriteEntry) *current {
	c := make(current, len(list))
	for _, e := range list {
		c.addAnswer(e.Domain, e.Answer)
	}
	return &c
}

func (c *current) has(domain, answer string) bool {
	return containsAnswer((*c)[domain], answer)
}

// family 同族（与 answer 同 IP 版本）现有答案。
func (c *current) family(domain, answer string) []string {
	v6 := isV6(answer)
	var out []string
	for _, a := range (*c)[domain] {
		if isV6(a) == v6 {
			out = append(out, a)
		}
	}
	return out
}

func (c *current) addAnswer(domain, answer string) {
	(*c)[domain] = append((*c)[domain], answer)
}

func (c *current) replaceAnswer(domain, old, new string) {
	lst := (*c)[domain]
	for i, a := range lst {
		if a == old {
			lst[i] = new
			return
		}
	}
}

func (c *current) removeAnswer(domain, answer string) {
	lst := (*c)[domain]
	for i, a := range lst {
		if a == answer {
			(*c)[domain] = append(lst[:i:i], lst[i+1:]...)
			return
		}
	}
}

func containsAnswer(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// isV6 依答案文本判断 IP 族（与 planner 同规则：含 ":" 即 v6）。
func isV6(answer string) bool {
	return strings.Contains(answer, ":")
}

// truncate 错误信息截断（rewrites.last_error 列防超长）。
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
