package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/aggregate"
	"cf-opt-adguard/internal/config"
)

// qlogEntry 构造 AGH querylog 记录的最小 JSON。
func qlogEntry(host, qtype, client, at string) map[string]any {
	return map[string]any{
		"question": map[string]string{"host": host, "type": qtype, "class": "IN"},
		"reason":   "NotFilteredNotFound",
		"client":   client,
		"time":     at,
	}
}

func qlogResp(entries []map[string]any) []byte {
	raw, _ := json.Marshal(map[string]any{"oldest": "", "data": entries})
	return raw
}

// fakeAGH 三页数据源：older_than 为空返回第 1 页，否则按页序推进。
type fakeAGH struct {
	pages [][]map[string]any
	seen  []string // 收到的 older_than 值
}

func (f *fakeAGH) handler(w http.ResponseWriter, r *http.Request) {
	f.seen = append(f.seen, r.URL.Query().Get("older_than"))
	idx := len(f.seen) - 1
	if idx >= len(f.pages) {
		_, _ = w.Write(qlogResp(nil))
		return
	}
	_, _ = w.Write(qlogResp(f.pages[idx]))
}

func newTestCollector(t *testing.T, srvURL string, mutate func(*config.Config)) (*Collector, *adguard.Client) {
	t.Helper()
	c := config.Default()
	c.AdGuard.URL = srvURL
	c.AdGuard.Username = "u"
	c.AdGuard.Password = "p"
	c.Detector.Resolvers = []string{"223.5.5.5"}
	c.CFIP.Source = "domain:one.one.one.one"
	if mutate != nil {
		mutate(c)
	}
	c.WindowDur, _ = config.ParseWindow(c.QueryLog.Window) // mutate 可能改窗口
	cli := adguard.New(srvURL, c.AdGuard.Username, c.AdGuard.Password, "", c.AdGuard.Timeout)
	return New(cli, c.WindowDur, c.QueryLog, slog.Default()), cli
}

func TestCollectPagingAndWindow(t *testing.T) {
	now := time.Now().UTC()
	inWin := func(minsAgo int) string { return now.Add(-time.Duration(minsAgo) * time.Minute).Format(time.RFC3339Nano) }
	outWin := now.Add(-30 * time.Hour).Format(time.RFC3339Nano) // 窗口外

	fake := &fakeAGH{pages: [][]map[string]any{
		{ // 第 1 页：新→旧，全部在窗口内（最早 100 分钟前）
			qlogEntry("a.example.com", "A", "192.168.1.50", inWin(5)),
			qlogEntry("b.example.com", "CNAME", "192.168.1.50", inWin(6)), // 非 A/AAAA → 丢
			qlogEntry("c.example.com", "A", "192.168.1.99", inWin(7)),     // 客户端 exclude → 丢
			qlogEntry("d.example.com", "A", "192.168.1.50", inWin(100)),
		},
		{ // 第 2 页：最旧一条已越界（30h > 24h 窗口）→ 采集后终止
			qlogEntry("e.example.com", "AAAA", "192.168.1.50", inWin(10 * 60)),
			qlogEntry("f.example.com", "A", "192.168.1.50", outWin),
		},
		{qlogEntry("never.example.com", "A", "192.168.1.50", inWin(1))}, // 不应被请求到
	}}

	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	c, _ := newTestCollector(t, srv.URL, func(cc *config.Config) {
		cc.QueryLog.Window = "24h"
		cc.QueryLog.PageSize = 500
		cc.QueryLog.ClientsExclude = []string{"192.168.1.99"}
	})

	entries, st, err := c.Collect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if st.Pages != 2 {
		t.Fatalf("应采集 2 页, got %d", st.Pages)
	}
	if st.Stopped != StopWindow {
		t.Fatalf("应因窗口终止, got %s", st.Stopped)
	}
	if st.Fetched != 6 {
		t.Fatalf("服务端条数应 6, got %d", st.Fetched)
	}
	if st.Kept != 3 { // a + d + e（b 非 A/AAAA、c 被客户端排除、f 窗口外）
		t.Fatalf("保留应 3 条, got %d: %+v", st.Kept, entries)
	}
	if len(entries) != 3 || entries[0].Host != "a.example.com" ||
		entries[1].Host != "d.example.com" || entries[2].Host != "e.example.com" {
		t.Fatalf("条目不符: %+v", entries)
	}
	// 翻页链：第 1 页无 older_than，第 2 页 older_than = 第 1 页最早记录（d，100 分钟前）
	if fake.seen[0] != "" {
		t.Fatalf("首页不应带 older_than: %q", fake.seen[0])
	}
	wantOlder := mustParse(t, inWin(100))
	if mustParse(t, fake.seen[1]) != wantOlder {
		t.Fatalf("第 2 页 older_than 应为第 1 页最早记录 %s, got %s", outWin, fake.seen[1])
	}
}

func TestCollectMaxPages(t *testing.T) {
	fake := &fakeAGH{pages: nil}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.seen = append(fake.seen, r.URL.Query().Get("older_than"))
		_, _ = w.Write(qlogResp([]map[string]any{
			qlogEntry("x.example.com", "A", "1.2.3.4", time.Now().UTC().Format(time.RFC3339Nano)),
		}))
	}))
	defer srv.Close()

	c, _ := newTestCollector(t, srv.URL, func(cc *config.Config) { cc.QueryLog.MaxPages = 3 })
	_, st, err := c.Collect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if st.Pages != 3 || st.Stopped != StopMaxPages {
		t.Fatalf("应 3 页后 max_pages 终止: %+v", st)
	}
}

func TestCollectFilters(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-time.Minute).Format(time.RFC3339Nano)
	fake := &fakeAGH{pages: [][]map[string]any{{
		qlogEntry("keep.example.com", "A", "10.0.0.1", at),
		qlogEntry("drop.corp", "A", "10.0.0.1", at),          // 域名 exclude *.corp
		qlogEntry("keep.me.example.com", "A", "10.0.0.1", at), // include 后缀命中
		qlogEntry("other.org", "A", "10.0.0.1", at),           // include 未命中 → 丢
	}}}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	c, _ := newTestCollector(t, srv.URL, func(cc *config.Config) {
		cc.QueryLog.ClientsInclude = []string{"10.0.0.1"}
	})
	filter := aggregate.NewDomainFilter([]string{"*.example.com"}, []string{"*.corp"})
	entries, st, err := c.Collect(context.Background(), filter)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if st.Kept != 2 {
		t.Fatalf("应保留 2 条, got %d: %+v", st.Kept, entries)
	}
	for _, e := range entries {
		if e.Host == "other.org" || e.Host == "drop.corp" {
			t.Fatalf("不该保留 %s", e.Host)
		}
	}
}

func TestCollectEmptyAndReasonParam(t *testing.T) {
	var lastQ map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastQ = r.URL.Query()
		_, _ = w.Write(qlogResp(nil))
	}))
	defer srv.Close()

	c, _ := newTestCollector(t, srv.URL, nil)
	entries, st, err := c.Collect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 0 || st.Stopped != StopEOF || st.Pages != 1 {
		t.Fatalf("空日志应 1 页 eof 终止: %+v", st)
	}
	if fmt.Sprint(lastQ["reason"]) != "[NotFilteredNotFound NotFilteredAllowList]" {
		t.Fatalf("reason 参数不符: %v", lastQ["reason"])
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("解析时间 %s: %v", s, err)
	}
	return tm
}
