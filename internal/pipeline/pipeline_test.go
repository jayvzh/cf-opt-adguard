package pipeline_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/config"
	"cf-opt-adguard/internal/pipeline"
	"cf-opt-adguard/internal/planner"
	"cf-opt-adguard/internal/state"
)

// 优选 IP 与 CF 测试网段（cfAPI 返回的 CIDR 必须包含该 IP，才能拿到 CIDR 信号）。
const (
	preferredIP = "104.21.88.54"
	testCIDR    = "104.16.0.0/12"
	probedHost  = "example.com"
)

// ---- httptest 假 AGH ----

type writeHits map[string]int

// newFakeAGH 启动假 AGH：querylog 返回 3 条 example.com；rewrite list 只有用户手工条目；
// 全部写端点计数（集成断言：dry-run 必须零调用），被调用时返回 500 以便暴露问题。
func newFakeAGH(t *testing.T) (url string, hits writeHits) {
	t.Helper()
	hits = writeHits{}
	mux := http.NewServeMux()
	mux.HandleFunc("/control/querylog", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("older_than") == "" {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			writeJSON(w, map[string]any{"oldest": now, "data": []map[string]any{
				qlogEntry(now), qlogEntry(now), qlogEntry(now),
			}})
			return
		}
		writeJSON(w, map[string]any{"oldest": "", "data": []any{}})
	})
	mux.HandleFunc("/control/rewrite/list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{"domain": "manual.test", "answer": "192.168.1.1", "enabled": true}, // 用户手工条目
		})
	})
	for _, p := range []string{
		"/control/rewrite/add",
		"/control/rewrite/update",
		"/control/rewrite/delete",
		"/control/set_rules",
	} {
		path := p
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			hits[r.Method+" "+path]++
			w.WriteHeader(http.StatusInternalServerError)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

func qlogEntry(now string) map[string]any {
	return map[string]any{
		"question": map[string]any{"host": probedHost, "type": "A"},
		"reason":   "NotFilteredNotFound",
		"client":   "192.168.1.5",
		"time":     now,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- 独立假 DNS ----

func startFakeDNS(t *testing.T, answers map[string][]dns.RR) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("假 DNS 启动失败: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		q := strings.TrimSuffix(strings.ToLower(r.Question[0].Name), ".")
		if ans, ok := answers[q]; ok {
			m.Answer = ans
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

// ---- 拦截型 Transport：CF 官方端点 → httptest；HTTPS 探测 → 伪造 cf-ray 响应 ----

type fakeRT struct{ cfAPI string }

func (f fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.URL.Host == "api.cloudflare.com":
		u := *req.URL
		u.Scheme = "http"
		u.Host = strings.TrimPrefix(f.cfAPI, "http://")
		r2 := req.Clone(req.Context())
		r2.URL = &u
		return http.DefaultTransport.RoundTrip(r2)
	case strings.HasSuffix(req.URL.Host, probedHost):
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{
				"Cf-Ray": []string{"8c2f"},
				"Server": []string{"cloudflare"},
			},
			Body:          io.NopCloser(strings.NewReader("")),
			ContentLength: 0,
			Request:       req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Status:     "404 Not Found",
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{},
		Body:          io.NopCloser(strings.NewReader("")),
		ContentLength: 0,
		Request:       req,
	}, nil
}

func newCFIPEndpoint(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/client/v4/ips", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"success": true,
			"result":  map[string]any{"ipv4_cidrs": []string{testCIDR}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// ---- 组装 ----

// setup 全链路环境：假 AGH + 假 DNS + 假 CF 端点 + 状态库 + result.csv 工作目录。
func setup(t *testing.T) (cfg *config.Config, db *state.DB, stdout *strings.Builder, hits writeHits) {
	t.Helper()
	tmp := t.TempDir()
	t.Chdir(tmp) // cfst:result.csv 相对工作目录（D17 部署形态）

	aghURL, hits := newFakeAGH(t)
	dnsAddr := startFakeDNS(t, map[string][]dns.RR{
		probedHost: {mustRR(t, probedHost+". 300 IN A "+preferredIP)},
	})

	cfg = config.Default()
	cfg.AdGuard.URL = aghURL
	cfg.AdGuard.Username = "admin"
	cfg.AdGuard.Password = "pass"
	cfg.WindowDur = 24 * time.Hour
	cfg.QueryLog.FetchTimeout = 10 * time.Second
	cfg.QueryLog.MaxPages = 5
	cfg.Aggregate.MinHits24h = 2 // example.com 3 条达阈
	cfg.Detector.Resolvers = []string{dnsAddr}
	cfg.Detector.Timeout = 2 * time.Second
	cfg.Detector.Concurrency = 4
	cfg.Runtime.DBPath = filepath.Join(tmp, "data", "state.db")
	cfg.CFIP.Source = "cfst:result.csv"

	// result.csv 夹具（CFST 产物形态：首行表头，第 0 列为 IP，顺序即优先级）。
	csvData := "IP 地址,已发送,已接收,丢包率,平均延迟,下载速度 (MB/s)\n" +
		preferredIP + ",4,4,0.00,1.23,1.50\n"
	if err := os.WriteFile("result.csv", []byte(csvData), 0o644); err != nil {
		t.Fatalf("写入 result.csv: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmp, "data"), 0o755); err != nil {
		t.Fatalf("创建数据目录: %v", err)
	}
	var err error
	db, err = state.Open(cfg.Runtime.DBPath, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("打开状态库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stdout = &strings.Builder{}
	return cfg, db, stdout, hits
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("输出缺少 %q:\n%s", sub, s)
	}
}

// ---- 集成测试 ----

// TestDryRunFullPipeline 全链路 dry-run：采集→聚合→探测→优选→计划→打印+落库，
// 并断言 AGH 写端点（含 set_rules）零调用（计划 §4.11）。
func TestDryRunFullPipeline(t *testing.T) {
	cfg, db, stdout, hits := setup(t)

	deps := pipeline.Deps{
		Client: adguard.New(cfg.AdGuard.URL, cfg.AdGuard.Username, cfg.AdGuard.Password,
			"", 10*time.Second),
		DB:     db,
		HC:     &http.Client{Transport: fakeRT{cfAPI: newCFIPEndpoint(t)}},
		Log:    slog.New(slog.DiscardHandler),
		Stdout: stdout,
	}

	ctx := context.Background()
	st, code, err := pipeline.Run(ctx, cfg, deps)
	if err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}
	if code != pipeline.ExitOK {
		t.Fatalf("退出码 = %d, 期望 %d（%s）", code, pipeline.ExitOK, stdout.String())
	}

	// 统计与计划：单 host zone → 单条精确 add，无 update/remove。
	if st.PreferredIP != preferredIP {
		t.Fatalf("优选 IP = %s, 期望 %s", st.PreferredIP, preferredIP)
	}
	if st.Confirmed != 1 {
		t.Fatalf("confirmed = %d, 期望 1", st.Confirmed)
	}
	if got := st.Plan.Count(planner.ActionAdd); got != 1 {
		t.Fatalf("add = %d, 期望 1（计划：%s）", got, stdout.String())
	}
	if st.Plan.Count(planner.ActionUpdate) != 0 || st.Plan.Count(planner.ActionRemove) != 0 {
		t.Fatalf("update/remove 应为 0: update=%d remove=%d",
			st.Plan.Count(planner.ActionUpdate), st.Plan.Count(planner.ActionRemove))
	}

	// 打印内容：dry 标识 + add 条目。
	out := stdout.String()
	mustContain(t, out, "[DRY-RUN]")
	mustContain(t, out, "--- add ---")
	mustContain(t, out, probedHost+" → "+preferredIP)

	// dry-run 铁律：AGH 写端点零调用。
	if len(hits) != 0 {
		t.Fatalf("AGH 写端点被调用: %v", hits)
	}

	// 落库断言：domains / rewrites(pending) / 托管集合为空。
	ds, err := db.ListDomains(ctx)
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(ds) != 1 || ds[0].Domain != probedHost || ds[0].HitCount != 3 || ds[0].Zone != probedHost {
		t.Fatalf("domains 落库不符: %+v", ds)
	}
	rw, err := db.GetRewrite(ctx, probedHost, preferredIP, 4)
	if err != nil {
		t.Fatalf("GetRewrite: %v", err)
	}
	if rw.State != "pending" || rw.AGHPresent {
		t.Fatalf("rewrites 应登记 pending 且 AGHPresent=false: %+v", rw)
	}
	managed, err := db.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	// 托管边界 = active+pending（ARCH §4.5）：本轮 pending 目标应入集合；手工条目 manual.test 不在其中。
	if len(managed) != 1 || !managed[probedHost] {
		t.Fatalf("托管集合应为 {example.com}: %v", managed)
	}
}

// TestDryRunNoPreferredIPExit2 result.csv 缺失 → 优选 IP 为空 → 退出码 2，
// 不产出任何计划、不打印（D17：优选 IP 为空中止，不生成 remove 计划）。
func TestDryRunNoPreferredIPExit2(t *testing.T) {
	cfg, db, stdout, hits := setup(t)
	if err := os.Remove("result.csv"); err != nil {
		t.Fatalf("删除 result.csv: %v", err)
	}

	deps := pipeline.Deps{
		Client: adguard.New(cfg.AdGuard.URL, cfg.AdGuard.Username, cfg.AdGuard.Password,
			"", 10*time.Second),
		DB:     db,
		HC:     &http.Client{Transport: fakeRT{cfAPI: newCFIPEndpoint(t)}},
		Log:    slog.New(slog.DiscardHandler),
		Stdout: stdout,
	}

	st, code, err := pipeline.Run(context.Background(), cfg, deps)
	if err == nil {
		t.Fatal("期望返回 ErrNoIP 错误")
	}
	if code != pipeline.ExitNoIP {
		t.Fatalf("退出码 = %d, 期望 %d", code, pipeline.ExitNoIP)
	}
	if !st.Plan.Empty() {
		t.Fatalf("优选 IP 为空时不应生成计划: %+v", st.Plan)
	}
	if stdout.String() != "" {
		t.Fatalf("不应打印计划:\n%s", stdout.String())
	}
	if len(hits) != 0 {
		t.Fatalf("AGH 写端点被调用: %v", hits)
	}
}

// ---- live（--apply）集成夹具：可变 store 的假 AGH + store 驱动的假 DNS ----

// aghStore 假 AGH 内存 rewrite 存储（写端点真实变更，DNS 应答随写操作联动，
// 模拟 AGH rewrite 生效行为）。
type aghStore struct {
	mu      sync.Mutex
	entries []aghEntry
	failAdd map[string]bool // domain → add 端点返回 500（故障注入）
	writes  int             // 写端点调用总次数（含失败）
}

type aghEntry struct{ Domain, Answer string }

func newAghStore(seed ...aghEntry) *aghStore {
	return &aghStore{entries: seed, failAdd: map[string]bool{}}
}

func (s *aghStore) list() []aghEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]aghEntry{}, s.entries...)
}

func (s *aghStore) add(domain, answer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.failAdd[domain] {
		return errInjected
	}
	s.entries = append(s.entries, aghEntry{domain, answer})
	return nil
}

func (s *aghStore) update(domain, oldAnswer, newAnswer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	for i, e := range s.entries {
		if e.Domain == domain && e.Answer == oldAnswer {
			s.entries[i].Answer = newAnswer
			return nil
		}
	}
	return nil // 幂等夹具：目标不存在视为成功
}

func (s *aghStore) del(domain, answer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	for i, e := range s.entries {
		if e.Domain == domain && e.Answer == answer {
			s.entries = append(s.entries[:i:i], s.entries[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *aghStore) writeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// resolve AGH rewrite 的 DNS 行为模拟：精确匹配条目返回应答 IP。
func (s *aghStore) resolve(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.entries {
		if e.Domain == name {
			out = append(out, e.Answer)
		}
	}
	return out
}

var errInjected = errorString("injected add failure")

type errorString string

func (e errorString) Error() string { return string(e) }

// newLiveAGH 可变假 AGH：rewrite 读写端点全部联动 store；querylog 与 dry 夹具同形。
func newLiveAGH(t *testing.T, store *aghStore) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/control/querylog", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("older_than") == "" {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			writeJSON(w, map[string]any{"oldest": now, "data": []map[string]any{
				qlogEntry(now), qlogEntry(now), qlogEntry(now),
			}})
			return
		}
		writeJSON(w, map[string]any{"oldest": "", "data": []any{}})
	})
	mux.HandleFunc("/control/rewrite/list", func(w http.ResponseWriter, r *http.Request) {
		type row struct {
			Domain  string `json:"domain"`
			Answer  string `json:"answer"`
			Enabled bool   `json:"enabled"`
		}
		entries := store.list()
		rows := make([]row, len(entries))
		for i, e := range entries {
			rows[i] = row{Domain: e.Domain, Answer: e.Answer, Enabled: true}
		}
		writeJSON(w, rows)
	})
	mux.HandleFunc("/control/rewrite/add", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Domain string `json:"domain"`
			Answer string `json:"answer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if err := store.add(p.Domain, p.Answer); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/control/rewrite/update", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Target struct {
				Domain string `json:"domain"`
				Answer string `json:"answer"`
			} `json:"target"`
			Update struct {
				Domain string `json:"domain"`
				Answer string `json:"answer"`
			} `json:"update"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if err := store.update(b.Target.Domain, b.Target.Answer, b.Update.Answer); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/control/rewrite/delete", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Domain string `json:"domain"`
			Answer string `json:"answer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if err := store.del(p.Domain, p.Answer); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// startLiveDNS store 驱动的假 DNS：先查静态表（detector 探测信号），再查
// AGH store（模拟 rewrite 生效后的应答）。
func startLiveDNS(t *testing.T, store *aghStore, static map[string][]dns.RR) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("假 DNS 启动失败: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		q := strings.TrimSuffix(strings.ToLower(r.Question[0].Name), ".")
		if ans, ok := static[q]; ok {
			m.Answer = ans
			_ = w.WriteMsg(m)
			return
		}
		for _, ip := range store.resolve(q) {
			if strings.Contains(ip, ":") {
				rr, _ := dns.NewRR(q + ". 300 IN AAAA " + ip)
				m.Answer = append(m.Answer, rr)
			} else {
				rr, _ := dns.NewRR(q + ". 300 IN A " + ip)
				m.Answer = append(m.Answer, rr)
			}
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().(*net.UDPAddr).String()
}

// setupLive live 全链路环境：可变 AGH store + store 驱动 DNS + 状态库。
// 预置托管旧条目 old.test → 1.0.0.1（期望被计划 remove）。
func setupLive(t *testing.T) (cfg *config.Config, db *state.DB, stdout *strings.Builder,
	store *aghStore, dnsAddr string) {

	t.Helper()
	tmp := t.TempDir()
	t.Chdir(tmp)

	store = newAghStore(aghEntry{"old.test", "1.0.0.1"})
	aghURL := newLiveAGH(t, store)
	dnsAddr = startLiveDNS(t, store, map[string][]dns.RR{
		probedHost: {mustRR(t, probedHost+". 300 IN A "+preferredIP)},
	})

	cfg = config.Default()
	cfg.AdGuard.URL = aghURL
	cfg.AdGuard.Username = "admin"
	cfg.AdGuard.Password = "pass"
	cfg.WindowDur = 24 * time.Hour
	cfg.QueryLog.FetchTimeout = 10 * time.Second
	cfg.QueryLog.MaxPages = 5
	cfg.Aggregate.MinHits24h = 2
	cfg.Detector.Resolvers = []string{dnsAddr}
	cfg.Detector.Timeout = 2 * time.Second
	cfg.Detector.Concurrency = 4
	cfg.Runtime.DBPath = filepath.Join(tmp, "data", "state.db")
	cfg.Runtime.Apply = true
	cfg.Sync.RateLimit = time.Millisecond // 加速集成测试
	cfg.Sync.Retry = 0
	cfg.CFIP.Source = "cfst:result.csv"

	csvData := "IP 地址,已发送,已接收,丢包率,平均延迟,下载速度 (MB/s)\n" +
		preferredIP + ",4,4,0.00,1.23,1.50\n"
	if err := os.WriteFile("result.csv", []byte(csvData), 0o644); err != nil {
		t.Fatalf("写入 result.csv: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "data"), 0o755); err != nil {
		t.Fatalf("创建数据目录: %v", err)
	}
	var err error
	db, err = state.Open(cfg.Runtime.DBPath, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("打开状态库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 预置托管旧条目状态行（active：old.test 在托管边界内 → 计划应产出 remove）。
	ctx := context.Background()
	if err := db.UpsertRewrite(ctx, state.Rewrite{
		Domain: "old.test", Answer: "1.0.0.1", IPVersion: 4,
		State: "active", AGHPresent: true, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("预置 old.test 状态行: %v", err)
	}

	stdout = &strings.Builder{}
	return cfg, db, stdout, store, dnsAddr
}

func liveDeps(t *testing.T, cfg *config.Config, db *state.DB, stdout *strings.Builder,
	dnsAddr string) pipeline.Deps {

	t.Helper()
	return pipeline.Deps{
		Client: adguard.New(cfg.AdGuard.URL, cfg.AdGuard.Username, cfg.AdGuard.Password,
			"", 10*time.Second),
		DB:      db,
		HC:      &http.Client{Transport: fakeRT{cfAPI: newCFIPEndpoint(t)}},
		Log:     slog.New(slog.DiscardHandler),
		Stdout:  stdout,
		DNSAddr: dnsAddr,
	}
}

// TestLiveApplyEndToEnd live 全链路：add example.com + remove 旧托管条目 old.test
// → syncer 写 AGH → 状态库对齐（active/removed）→ verifier 回查通过 → 退出码 0。
func TestLiveApplyEndToEnd(t *testing.T) {
	cfg, db, stdout, store, dnsAddr := setupLive(t)

	st, code, err := pipeline.Run(context.Background(), cfg, liveDeps(t, cfg, db, stdout, dnsAddr))
	if err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}
	if code != pipeline.ExitOK {
		t.Fatalf("退出码 = %d, 期望 %d\n%s", code, pipeline.ExitOK, stdout.String())
	}

	// 计划：add example.com + remove old.test。
	if got := st.Plan.Count(planner.ActionAdd); got != 1 {
		t.Fatalf("add = %d, 期望 1\n%s", got, stdout.String())
	}
	if got := st.Plan.Count(planner.ActionRemove); got != 1 {
		t.Fatalf("remove = %d, 期望 1\n%s", got, stdout.String())
	}

	// 同步与回查汇总。
	if st.Sync.OK != 2 || st.Sync.Fail != 0 {
		t.Fatalf("同步汇总 = %+v, 期望 OK=2 Fail=0", st.Sync)
	}
	if st.Verify.Verified != 1 || len(st.Verify.Failed) != 0 {
		t.Fatalf("回查汇总 = %+v, 期望 Verified=1 Failed=0", st.Verify)
	}

	// AGH store 实际变更：old.test 已删、example.com 已写。
	entries := store.list()
	want := map[string]string{probedHost: preferredIP}
	for _, e := range entries {
		if e.Domain == "old.test" {
			t.Fatalf("old.test 应已被删除: %+v", entries)
		}
		want[e.Domain] = e.Answer
	}
	if want[probedHost] != preferredIP {
		t.Fatalf("AGH store 应含 %s → %s: %+v", probedHost, preferredIP, entries)
	}

	// 状态库对齐：example.com active + AGHPresent；old.test removed 归档。
	ctx := context.Background()
	rw, err := db.GetRewrite(ctx, probedHost, preferredIP, 4)
	if err != nil {
		t.Fatalf("GetRewrite(example.com): %v", err)
	}
	if rw.State != "active" || !rw.AGHPresent || rw.FirstSynced.IsZero() {
		t.Fatalf("example.com 状态行应为 active: %+v", rw)
	}
	rwOld, err := db.GetRewrite(ctx, "old.test", "1.0.0.1", 4)
	if err != nil {
		t.Fatalf("GetRewrite(old.test): %v", err)
	}
	if rwOld.State != "removed" || rwOld.AGHPresent {
		t.Fatalf("old.test 状态行应归档 removed: %+v", rwOld)
	}

	// 打印：live 标识 + 同步/验证摘要 + 执行结果语义（非“变更计划”视角）。
	out := stdout.String()
	mustContain(t, out, "[APPLY]")
	mustContain(t, out, "同步: 成功 2 / 失败 0")
	mustContain(t, out, "回查验证: 通过 1 / 失败 0")
	mustContain(t, out, "执行结果: add 1 / update 0 / remove 1")
	mustContain(t, out, "--- remove ---")
	mustContain(t, out, probedHost+" → "+preferredIP+"（写入成功，回查验证通过）")
	mustContain(t, out, "old.test → 1.0.0.1（移除成功，回查验证通过）")
	if strings.Contains(out, "变更计划:") || strings.Contains(out, "AGH 无此条目") {
		t.Fatalf("live 输出不应再使用计划前视角:\n%s", out)
	}
}

// TestLivePartialFailureExit4 add 端点持续 500 → 单条最终失败不中断整批
// （remove 仍成功）→ 退出码 4，失败条目 last_error 落库。
func TestLivePartialFailureExit4(t *testing.T) {
	cfg, db, stdout, store, dnsAddr := setupLive(t)
	store.mu.Lock()
	store.failAdd[probedHost] = true
	store.mu.Unlock()

	st, code, err := pipeline.Run(context.Background(), cfg, liveDeps(t, cfg, db, stdout, dnsAddr))
	if err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}
	if code != pipeline.ExitPartial {
		t.Fatalf("退出码 = %d, 期望 %d\n%s", code, pipeline.ExitPartial, stdout.String())
	}

	if st.Sync.OK != 1 || st.Sync.Fail != 1 || len(st.Sync.Failed) != 1 {
		t.Fatalf("同步汇总 = %+v, 期望 OK=1（remove 成功）Fail=1", st.Sync)
	}
	if st.Sync.Failed[0].Domain != probedHost {
		t.Fatalf("失败条目应为 %s: %+v", probedHost, st.Sync.Failed)
	}

	// 失败条目：状态保留 pending，last_error 记录 add 失败。
	ctx := context.Background()
	rw, err := db.GetRewrite(ctx, probedHost, preferredIP, 4)
	if err != nil {
		t.Fatalf("GetRewrite: %v", err)
	}
	if rw.State != "pending" || !strings.Contains(rw.LastError, "add") {
		t.Fatalf("失败条目应保留 pending 且记 last_error: %+v", rw)
	}
	// remove 条目不受影响，仍成功。
	rwOld, err := db.GetRewrite(ctx, "old.test", "1.0.0.1", 4)
	if err != nil {
		t.Fatalf("GetRewrite(old.test): %v", err)
	}
	if rwOld.State != "removed" {
		t.Fatalf("remove 不应受单条失败影响: %+v", rwOld)
	}

	out := stdout.String()
	mustContain(t, out, "--- 同步失败")
	mustContain(t, out, probedHost+" → "+preferredIP+"（写入失败（last_error 已落库，下轮重试））")
	mustContain(t, out, "old.test → 1.0.0.1（移除成功，回查验证通过）")
}

// TestLiveConfirmCancelled Confirm 钩子返回 false（用户在写入确认时取消）：
// 退出码 5，AGH 零写调用（add/remove 均未执行），托管状态行不被降级。
func TestLiveConfirmCancelled(t *testing.T) {
	cfg, db, stdout, store, dnsAddr := setupLive(t)

	confirmed := false
	deps := liveDeps(t, cfg, db, stdout, dnsAddr)
	deps.Confirm = func(p planner.Plan) bool {
		confirmed = true
		return false
	}

	st, code, err := pipeline.Run(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}
	if code != pipeline.ExitCancelled {
		t.Fatalf("退出码 = %d, 期望 %d\n%s", code, pipeline.ExitCancelled, stdout.String())
	}
	if !confirmed {
		t.Fatal("Confirm 钩子未被调用")
	}

	// 零写操作：AGH store 仅剩预置 old.test，example.com 未写入。
	entries := store.list()
	if len(entries) != 1 || entries[0].Domain != "old.test" {
		t.Fatalf("取消后 AGH 应保持原状（仅 old.test）: %+v", entries)
	}

	// 状态库：old.test 仍 active（未执行 remove）；example.com 计划行停留 pending。
	ctx := context.Background()
	rwOld, err := db.GetRewrite(ctx, "old.test", "1.0.0.1", 4)
	if err != nil {
		t.Fatalf("GetRewrite(old.test): %v", err)
	}
	if rwOld.State != "active" || !rwOld.AGHPresent {
		t.Fatalf("取消后 old.test 应保持 active: %+v", rwOld)
	}
	rw, err := db.GetRewrite(ctx, probedHost, preferredIP, 4)
	if err != nil {
		t.Fatalf("GetRewrite(example.com): %v", err)
	}
	if rw.State != "pending" {
		t.Fatalf("取消后 example.com 应停留 pending（计划已产出未同步）: %+v", rw)
	}
	if st.Sync.OK != 0 || st.Sync.Fail != 0 {
		t.Fatalf("取消后同步汇总应为零值: %+v", st.Sync)
	}
}
