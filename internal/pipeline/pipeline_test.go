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
		StatusCode:    http.StatusNotFound,
		Status:        "404 Not Found",
		Proto:         "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
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
