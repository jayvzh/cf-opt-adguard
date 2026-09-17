// Package verifier 同步完成后经 AGH DNS 回查验证写入结果（ARCHITECTURE §4.7）。
// 对计划中的 add / update 条目向 AGH 发起 DNS 查询，比对应答是否包含目标 answer；
// 失败条目保留原状态仅记 last_error（active 不降级，避免下轮误判为未同步）。
package verifier

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	"cf-opt-adguard/internal/planner"
	"cf-opt-adguard/internal/state"
)

// verifyLabel wildcard 条目回查使用的测试子域标签（AGH wildcard rewrite 仅匹配子域）。
const verifyLabel = "cf-opt-verify"

// Verifier AGH DNS 回查器。
type Verifier struct {
	server  string // AGH DNS 地址（host:53，由调用方提供）
	timeout time.Duration
	log     *slog.Logger
}

// New 创建回查器。server 为 AGH DNS 服务地址（host:port）。
func New(server string, timeout time.Duration, log *slog.Logger) *Verifier {
	if log == nil {
		log = slog.Default()
	}
	return &Verifier{server: server, timeout: timeout, log: log}
}

// Result 一次回查的汇总。
type Result struct {
	Verified int              // 通过条数
	Failed   []planner.Entry  // 回查未通过条目（已记 last_error）
	Aborted  bool             // ctx 取消中止（剩余条目未验证，不计失败）
}

// VerifyAll 逐条回查全部条目（串行：本地 DNS 快，且写入阶段已限速）。
func (v *Verifier) VerifyAll(ctx context.Context, db *state.DB, entries []planner.Entry) Result {
	var res Result
	for _, e := range entries {
		if ctx.Err() != nil {
			res.Aborted = true
			return res
		}
		if err := v.Verify(ctx, e); err != nil {
			v.log.Warn("rewrite 回查失败", "domain", e.Domain, "answer", e.Answer, "err", err)
			res.Failed = append(res.Failed, e)
			v.markFail(ctx, db, e, err)
			continue
		}
		res.Verified++
	}
	return res
}

// Verify 回查单条：向 AGH 查询该域名（wildcard 换测试子域），应答须含目标 answer。
func (v *Verifier) Verify(ctx context.Context, e planner.Entry) error {
	name := strings.TrimPrefix(e.Domain, "*.")
	if name != e.Domain {
		name = verifyLabel + "." + name
	}
	qtype := dns.TypeA
	if e.IPVersion == 6 {
		qtype = dns.TypeAAAA
	}
	ips, err := v.query(ctx, name, qtype)
	if err != nil {
		return err
	}
	want := net.ParseIP(e.Answer)
	for _, ip := range ips {
		if want != nil && ip.Equal(want) {
			return nil
		}
	}
	return fmt.Errorf("应答不含目标 %s（实际 %d 条）", e.Answer, len(ips))
}

// query 向 AGH 发起 UDP DNS 查询，返回应答中的 A / AAAA 记录。
func (v *Verifier) query(ctx context.Context, name string, qtype uint16) ([]net.IP, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.RecursionDesired = true
	cl := &dns.Client{Timeout: v.timeout, Net: "udp"}
	in, _, err := cl.ExchangeContext(ctx, m, v.server)
	if err != nil {
		return nil, err
	}
	if in.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("DNS 响应码 %s", dns.RcodeToString[in.Rcode])
	}
	var ips []net.IP
	for _, a := range in.Answer {
		switch rr := a.(type) {
		case *dns.A:
			ips = append(ips, rr.A)
		case *dns.AAAA:
			ips = append(ips, rr.AAAA)
		}
	}
	return ips, nil
}

// markFail 回查失败条目：保留原状态，仅记 last_error。
func (v *Verifier) markFail(ctx context.Context, db *state.DB, e planner.Entry, err error) {
	st := "pending"
	if rw, gerr := db.GetRewrite(ctx, e.Domain, e.Answer, e.IPVersion); gerr == nil {
		st = rw.State
	}
	msg := truncate("verify: "+err.Error(), 500)
	if serr := db.SetRewriteState(ctx, e.Domain, e.Answer, e.IPVersion, st, msg); serr != nil {
		v.log.Error("记录 rewrite 回查失败出错", "domain", e.Domain, "err", serr)
	}
}

// truncate 错误信息截断（rewrites.last_error 列防超长）。
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
