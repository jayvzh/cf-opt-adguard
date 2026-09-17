package verifier

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"cf-opt-adguard/internal/planner"
	"cf-opt-adguard/internal/state"
)

// startFakeDNS 启动假 DNS（answers: 域名 → 应答 IP 文本列表）。
func startFakeDNS(t *testing.T, answers map[string][]string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("假 DNS 启动失败: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		q := strings.TrimSuffix(strings.ToLower(r.Question[0].Name), ".")
		for _, ip := range answers[q] {
			if strings.Contains(ip, ":") {
				m.Answer = append(m.Answer, mustRR(t, q+". 300 IN AAAA "+ip))
			} else {
				m.Answer = append(m.Answer, mustRR(t, q+". 300 IN A "+ip))
			}
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().(*net.UDPAddr).String()
}

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(s)
	if err != nil {
		t.Fatalf("构造 RR %q: %v", s, err)
	}
	return rr
}

func newTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("打开状态库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustUpsert(t *testing.T, db *state.DB, r state.Rewrite) {
	t.Helper()
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = time.Now()
	}
	if err := db.UpsertRewrite(context.Background(), r); err != nil {
		t.Fatalf("UpsertRewrite %s: %v", r.Domain, err)
	}
}

func newVerifier(server string) *Verifier {
	return &Verifier{server: server, timeout: 500 * time.Millisecond, log: slog.New(slog.DiscardHandler)}
}

// TestVerifyV4AndV6 A / AAAA 两族命中目标 answer。
func TestVerifyV4AndV6(t *testing.T) {
	srv := startFakeDNS(t, map[string][]string{
		"a.test": {"1.1.1.1", "3.3.3.3"},
		"b.test": {"2606:4700::1"},
	})
	v := newVerifier(srv)

	if err := v.Verify(context.Background(), planner.Entry{Domain: "a.test", Answer: "3.3.3.3", IPVersion: 4}); err != nil {
		t.Fatalf("v4 命中应通过: %v", err)
	}
	if err := v.Verify(context.Background(), planner.Entry{Domain: "b.test", Answer: "2606:4700::1", IPVersion: 6}); err != nil {
		t.Fatalf("v6 命中应通过: %v", err)
	}
}

// TestVerifyWildcard wildcard 条目回查测试子域 <label>.<zone>。
func TestVerifyWildcard(t *testing.T) {
	zone := "a.test"
	srv := startFakeDNS(t, map[string][]string{
		verifyLabel + "." + zone: {"3.3.3.3"},
	})
	v := newVerifier(srv)

	if err := v.Verify(context.Background(), planner.Entry{Domain: "*." + zone, Answer: "3.3.3.3", IPVersion: 4}); err != nil {
		t.Fatalf("wildcard 回查应通过: %v", err)
	}
}

// TestVerifyMismatchMismatch 应答不含目标 answer → 失败；状态保留原值仅记 last_error。
func TestVerifyMismatch(t *testing.T) {
	srv := startFakeDNS(t, map[string][]string{
		"a.test": {"1.1.1.1"},
	})
	v := newVerifier(srv)
	db := newTestDB(t)
	mustUpsert(t, db, state.Rewrite{Domain: "a.test", Answer: "3.3.3.3", IPVersion: 4, State: "active", AGHPresent: true})

	res := v.VerifyAll(context.Background(), db, []planner.Entry{
		{Domain: "a.test", Answer: "3.3.3.3", IPVersion: 4},
	})
	if res.Verified != 0 || len(res.Failed) != 1 {
		t.Fatalf("结果 = %+v, 期望 1 条失败", res)
	}
	rw, err := db.GetRewrite(context.Background(), "a.test", "3.3.3.3", 4)
	if err != nil {
		t.Fatal(err)
	}
	if rw.State != "active" || !strings.Contains(rw.LastError, "verify") {
		t.Fatalf("状态应保留 active 且记 last_error: %+v", rw)
	}
}

// TestVerifyDNSTimeout DNS 无响应 → 短超时失败落库。
func TestVerifyDNSTimeout(t *testing.T) {
	// 127.0.0.1 上无监听端口：UDP 通常静默丢包触发超时路径。
	v := &Verifier{server: "127.0.0.1:1", timeout: 100 * time.Millisecond, log: slog.New(slog.DiscardHandler)}
	db := newTestDB(t)
	mustUpsert(t, db, state.Rewrite{Domain: "a.test", Answer: "3.3.3.3", IPVersion: 4, State: "pending"})

	res := v.VerifyAll(context.Background(), db, []planner.Entry{
		{Domain: "a.test", Answer: "3.3.3.3", IPVersion: 4},
	})
	if len(res.Failed) != 1 {
		t.Fatalf("不可达 DNS 应判失败: %+v", res)
	}
	rw, err := db.GetRewrite(context.Background(), "a.test", "3.3.3.3", 4)
	if err != nil {
		t.Fatal(err)
	}
	if rw.State != "pending" || rw.LastError == "" {
		t.Fatalf("状态保留 pending 且记 last_error: %+v", rw)
	}
}

// TestVerifyAllAborted ctx 取消 → 中止，剩余条目不计失败。
func TestVerifyAllAborted(t *testing.T) {
	srv := startFakeDNS(t, map[string][]string{"a.test": {"3.3.3.3"}})
	v := newVerifier(srv)
	db := newTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := v.VerifyAll(ctx, db, []planner.Entry{
		{Domain: "a.test", Answer: "3.3.3.3", IPVersion: 4},
		{Domain: "b.test", Answer: "3.3.3.3", IPVersion: 4},
	})
	if !res.Aborted || res.Verified != 0 || len(res.Failed) != 0 {
		t.Fatalf("取消后应中止且不计失败: %+v", res)
	}
}

// TestNewServerPassthrough New 原样持有传入的 server 地址（host:port）。
func TestNewServerPassthrough(t *testing.T) {
	v := New("192.168.1.2:53", time.Second, nil)
	if v.server != "192.168.1.2:53" {
		t.Fatalf("server = %s, 期望原样传入的 192.168.1.2:53", v.server)
	}
	if v.log == nil {
		t.Fatal("log 为 nil 时应回退默认日志")
	}
}
