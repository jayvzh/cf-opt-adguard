package detector

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"cf-opt-adguard/internal/aggregate"
	"cf-opt-adguard/internal/config"
)

func testCfg() config.DetectorConfig {
	return config.DetectorConfig{
		Resolvers:      []string{"127.0.0.1:0"}, // 占位，各测试自行覆盖
		Concurrency:    4,
		Timeout:        300 * time.Millisecond,
		HTTPEnabled:    false,
		ScoreThreshold: 4,
	}
}

func TestScore(t *testing.T) {
	cases := []struct {
		sig  Signal
		want int
	}{
		{Signal{}, 0},
		{Signal{CNAMECF: true}, 2},
		{Signal{CIDR: true}, 2},
		{Signal{CFRay: true}, 3},
		{Signal{ServerCF: true}, 1},
		{Signal{CNAMECF: true, CIDR: true, CFRay: true, ServerCF: true}, 8},
	}
	for _, c := range cases {
		if got := Score(c.sig); got != c.want {
			t.Errorf("Score(%+v)=%d want %d", c.sig, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	if got := Classify(4, 4); got != aggregate.VerdictConfirmed {
		t.Errorf("score=4 threshold=4 → %s want confirmed", got)
	}
	if got := Classify(3, 4); got != aggregate.VerdictMaybe {
		t.Errorf("score=3 threshold=4 → %s want maybe", got)
	}
	if got := Classify(0, 4); got != aggregate.VerdictNotCF {
		t.Errorf("score=0 → %s want not_cf", got)
	}
}

// startFakeDNS 起本地 UDP 假 DNS，按记录表应答；返回 resolver 地址。
func startFakeDNS(t *testing.T, answers map[string][]dns.RR) string {
	t.Helper()
	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		name := strings.ToLower(r.Question[0].Name)
		if rrs, ok := answers[name]; ok {
			m.Answer = rrs
		} else {
			m.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(m)
	})
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dns: %v", err)
	}
	server := &dns.Server{PacketConn: pc, Handler: mux}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	return pc.LocalAddr().String()
}

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(s)
	if err != nil {
		t.Fatalf("构造 RR %q: %v", s, err)
	}
	return rr
}

var cfTestCIDRs = func() []*net.IPNet {
	nets, err := parseCIDRs([]string{"104.16.0.0/12", "173.245.48.0/20"})
	if err != nil {
		panic(err)
	}
	return nets
}()

func TestDetectChainCNAMEAndCIDR(t *testing.T) {
	addr := startFakeDNS(t, map[string][]dns.RR{
		"example.com.": {mustRR(t, "example.com. 300 IN CNAME host.examplecdn.cloudflare.net.")},
		"host.examplecdn.cloudflare.net.": {
			mustRR(t, "host.examplecdn.cloudflare.net. 300 IN A 104.16.132.229"),
		},
	})
	cfg := testCfg()
	cfg.Resolvers = []string{addr}
	d := New(cfg, http.DefaultClient, nil)
	d.SetCIDRs(cfTestCIDRs)

	res := d.Detect(context.Background(), "example.com")
	if res.Err != "" {
		t.Fatalf("意外错误: %s", res.Err)
	}
	if !res.Signal.CNAMECF || !res.Signal.CIDR {
		t.Errorf("信号不全: %+v", res.Signal)
	}
	if res.Score != 4 {
		t.Errorf("score=%d want 4", res.Score)
	}
	if res.Verdict != aggregate.VerdictConfirmed {
		t.Errorf("verdict=%s want confirmed", res.Verdict)
	}
}

func TestDetectMultiAOnlyOneInCF(t *testing.T) {
	addr := startFakeDNS(t, map[string][]dns.RR{
		"mixed.example.com.": {
			mustRR(t, "mixed.example.com. 300 IN A 1.2.3.4"),
			mustRR(t, "mixed.example.com. 300 IN A 104.16.0.1"),
		},
	})
	cfg := testCfg()
	cfg.Resolvers = []string{addr}
	d := New(cfg, http.DefaultClient, nil)
	d.SetCIDRs(cfTestCIDRs)

	res := d.Detect(context.Background(), "mixed.example.com")
	if res.Score != 2 || res.Verdict != aggregate.VerdictMaybe {
		t.Errorf("score=%d verdict=%s want 2/maybe", res.Score, res.Verdict)
	}
}

func TestDetectDNSFailureIsNotCF(t *testing.T) {
	cfg := testCfg()
	cfg.Resolvers = []string{"127.0.0.1:1"} // 不可达
	cfg.Timeout = 100 * time.Millisecond
	d := New(cfg, http.DefaultClient, nil)

	res := d.Detect(context.Background(), "dead.example.com")
	if res.Verdict != aggregate.VerdictNotCF || res.Err == "" {
		t.Errorf("verdict=%s err=%q want not_cf+err", res.Verdict, res.Err)
	}
}

func TestDetectHTTPHeaders(t *testing.T) {
	addr := startFakeDNS(t, map[string][]dns.RR{
		"hdr.example.com.": {mustRR(t, "hdr.example.com. 300 IN A 104.16.0.7")},
	})
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Cf-Ray": []string{"8f2a-ARN"},
				"Server": []string{"Cloudflare"},
			},
			Body:          http.NoBody,
			Request:       req,
		}
		return resp, nil
	})
	hc := &http.Client{Transport: rt, Timeout: time.Second}

	cfg := testCfg()
	cfg.Resolvers = []string{addr}
	cfg.HTTPEnabled = true
	d := New(cfg, hc, nil)
	d.SetCIDRs(cfTestCIDRs)

	res := d.Detect(context.Background(), "hdr.example.com")
	if !res.Signal.CFRay || !res.Signal.ServerCF {
		t.Errorf("信号: %+v want cf-ray+server", res.Signal)
	}
	if res.Score != 2+3+1 {
		t.Errorf("score=%d want 6", res.Score)
	}
}

// roundTripFunc 便捷假 Transport。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProbeHTTPHeadFallbackGet(t *testing.T) {
	methods := []string{}
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		methods = append(methods, req.Method)
		if req.Method == http.MethodHead {
			return nil, fmt.Errorf("HEAD 不支持")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Cf-Ray": []string{"x"}},
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})
	d := New(testCfg(), &http.Client{Transport: rt}, nil)
	cfRay, serverCF := d.probeHTTP(context.Background(), "fallback.example.com")
	if len(methods) != 2 || methods[0] != http.MethodHead || methods[1] != http.MethodGet {
		t.Errorf("调用序列 %v want HEAD→GET", methods)
	}
	if !cfRay || serverCF {
		t.Errorf("cfRay=%v serverCF=%v want true/false", cfRay, serverCF)
	}
}

func TestDetectAllOrderAndConcurrency(t *testing.T) {
	hosts := []string{
		"plain1.example.com", "cf1.example.com", "plain2.example.com",
		"cf2.example.com", "plain3.example.com",
	}
	answers := map[string][]dns.RR{}
	for _, h := range hosts {
		ip := "1.2.3.4"
		if strings.HasPrefix(h, "cf") {
			ip = "104.16.0.9"
		}
		answers[h+"."] = []dns.RR{mustRR(t, fmt.Sprintf("%s. 300 IN A %s", h, ip))}
	}
	addr := startFakeDNS(t, answers)
	cfg := testCfg()
	cfg.Resolvers = []string{addr}
	d := New(cfg, http.DefaultClient, nil)
	d.SetCIDRs(cfTestCIDRs)

	results := d.DetectAll(context.Background(), hosts)
	if len(results) != len(hosts) {
		t.Fatalf("结果数 %d want %d", len(results), len(hosts))
	}
	for i, res := range results {
		if res.Host != hosts[i] {
			t.Fatalf("顺序错乱: results[%d].Host=%s want %s", i, res.Host, hosts[i])
		}
		want := aggregate.VerdictNotCF
		if strings.HasPrefix(hosts[i], "cf") {
			want = aggregate.VerdictMaybe
		}
		if res.Verdict != want {
			t.Errorf("%s verdict=%s want %s", hosts[i], res.Verdict, want)
		}
	}
}

func TestLoadCFIPsFetchAndCache(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"ipv4_cidrs": []string{"104.16.0.0/12", "173.245.48.0/20"},
				"ipv6_cidrs": []string{"2400:cb00::/32"},
			},
		})
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "sub", "cf_ips.json")

	// 端点替换为假服务器：LoadCFIPs 走入参 hc，端点常量不可变 →
	// P0 直接用假 Transport 把官方域名重写到假服务器。
	rewrite := func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = srv.URL[len("http://"):]
			req.Host = srv.URL[len("http://"):]
			return next.RoundTrip(req)
		})
	}
	hc := &http.Client{Transport: rewrite(http.DefaultTransport)}

	nets, err := LoadCFIPs(context.Background(), hc, cachePath, []int{4, 6}, nil)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(nets) != 3 {
		t.Fatalf("全版本条数 %d want 3", len(nets))
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("缓存未落盘: %v", err)
	}

	// 第二次：走新鲜缓存 + 版本重过滤，不再请求端点。
	nets4, err := LoadCFIPs(context.Background(), hc, cachePath, []int{4}, nil)
	if err != nil || len(nets4) != 2 {
		t.Fatalf("v4 缓存重过滤: nets=%d err=%v", len(nets4), err)
	}
	nets6, err := LoadCFIPs(context.Background(), hc, cachePath, []int{6}, nil)
	if err != nil || len(nets6) != 1 {
		t.Fatalf("v6 缓存重过滤: nets=%d err=%v", len(nets6), err)
	}
	if calls != 1 {
		t.Fatalf("应命中缓存: calls=%d", calls)
	}
}

func TestLoadCFIPsFallbackStaleCache(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cf_ips.json")
	// 预置 8 天前的过期缓存。
	stale := cidrCache{
		FetchedAt: time.Now().UTC().Add(-8 * 24 * time.Hour),
		CIDRs:     []string{"104.16.0.0/12"},
	}
	data, _ := json.Marshal(stale)
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("网络不可达")
	})}

	nets, err := LoadCFIPs(context.Background(), hc, cachePath, []int{4}, nil)
	if err != nil || len(nets) != 1 {
		t.Fatalf("应回退过期缓存: nets=%d err=%v", len(nets), err)
	}

	// 无缓存时错误透传。
	_, err = LoadCFIPs(context.Background(), hc, filepath.Join(t.TempDir(), "none.json"), []int{4}, nil)
	if err == nil {
		t.Fatal("无缓存拉取失败应返回错误")
	}
}

func TestNormalizeResolver(t *testing.T) {
	cases := map[string]string{
		"1.1.1.1":       "1.1.1.1:53",
		"1.1.1.1:5353":  "1.1.1.1:5353",
		"2606:4700:4700::1111": "[2606:4700:4700::1111]:53",
	}
	for in, want := range cases {
		if got := normalizeResolver(in); got != want {
			t.Errorf("normalizeResolver(%q)=%q want %q", in, got, want)
		}
	}
}
