package adguard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// serve 夹具：逐请求回调断言，resp 由测试自定。
type capture struct {
	r *http.Request
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取夹具: %v", err)
	}
	return string(raw)
}

func TestBasicAuthQueryLog(t *testing.T) {
	var cap capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.r = r
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(readFile(t, "querylog_page.json")))
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "pass", AuthBasic, 5*time.Second)
	page, err := c.QueryLogPage(context.Background(), QueryLogParams{Limit: 500})
	if err != nil {
		t.Fatalf("QueryLogPage: %v", err)
	}
	// Basic 头断言
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:pass"))
	if got := cap.r.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization 头不符: %s", got)
	}
	if cap.r.URL.Path != "/control/querylog" {
		t.Fatalf("路径不符: %s", cap.r.URL.Path)
	}
	// 夹具解析断言
	if page.Oldest != "2026-09-10T00:00:00Z" {
		t.Fatalf("oldest 不符: %s", page.Oldest)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("应解析出 2 条, got %d", len(page.Entries))
	}
	e := page.Entries[0]
	if e.Question.Host != "assets.example.com" || e.Question.Type != "A" ||
		e.Reason != "NotFilteredNotFound" || e.Client != "192.168.1.50" {
		t.Fatalf("首条字段不符: %+v", e)
	}
}

func TestQueryLogParams(t *testing.T) {
	var gotQ url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQ = r.URL.Query()
		_, _ = w.Write([]byte(`{"oldest":"","data":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "u", "p", AuthBasic, 5*time.Second)
	older := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	_, err := c.QueryLogPage(context.Background(), QueryLogParams{
		Limit:     500,
		OlderThan: older,
		Reasons:   []string{"NotFilteredNotFound", "NotFilteredAllowList"},
	})
	if err != nil {
		t.Fatalf("QueryLogPage: %v", err)
	}
	if gotQ.Get("limit") != "500" {
		t.Fatalf("limit 不符: %v", gotQ)
	}
	wantOlder := older.UTC().Format(time.RFC3339Nano)
	if gotQ.Get("older_than") != wantOlder {
		t.Fatalf("older_than 应为 RFC3339Nano %s, got %s", wantOlder, gotQ.Get("older_than"))
	}
	if len(gotQ["reason"]) != 2 {
		t.Fatalf("reason 应可多值: %v", gotQ["reason"])
	}
}

func TestQueryLogLegacyReasonFallback(t *testing.T) {
	var gotQ url.Values
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotQ = r.URL.Query()
		if gotQ["reason"] != nil && slices.Contains(gotQ["reason"], "NotFilteredAllowList") {
			// 模拟旧版 AGH：不认识新枚举值，返回 400（报文与真实实例一致）
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`parsing params: reason: bad enum value: "NotFilteredAllowList"`))
			return
		}
		_, _ = w.Write([]byte(`{"oldest":"","data":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "u", "p", AuthBasic, 5*time.Second)
	params := QueryLogParams{Limit: 10, Reasons: []string{"NotFilteredNotFound", "NotFilteredAllowList"}}

	// 首页：新值 400 → 自动降级旧拼写重试成功
	if _, err := c.QueryLogPage(context.Background(), params); err != nil {
		t.Fatalf("降级重试后应成功: %v", err)
	}
	if calls != 2 {
		t.Fatalf("首次调用应恰好请求 2 次（400+降级重试）, got %d", calls)
	}
	if !slices.Contains(gotQ["reason"], "NotFilteredWhiteList") {
		t.Fatalf("重试应使用旧拼写, got %v", gotQ["reason"])
	}

	// 次页：降级状态已记住，直接用旧值一次成功
	if _, err := c.QueryLogPage(context.Background(), params); err != nil {
		t.Fatalf("次页应直接成功: %v", err)
	}
	if calls != 3 {
		t.Fatalf("次页应只请求 1 次, got %d", calls)
	}
	if slices.Contains(gotQ["reason"], "NotFilteredAllowList") {
		t.Fatalf("次页不应再发新枚举值, got %v", gotQ["reason"])
	}
}

func TestCookieAuthLogin(t *testing.T) {
	logins := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/login":
			logins++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "admin" || body["password"] != "pass" {
				t.Errorf("登录体不符: %+v", body)
			}
			http.SetCookie(w, &http.Cookie{Name: "agh_session", Value: "sess1"})
			w.WriteHeader(http.StatusOK)
		case "/control/rewrite/list":
			if ck, err := r.Cookie("agh_session"); err != nil || ck.Value != "sess1" {
				t.Errorf("后续请求应携带会话 Cookie, got %v %v", ck, err)
			}
			_, _ = w.Write([]byte(`[{"domain":"a.example.com","answer":"104.16.1.1","enabled":true}]`))
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "pass", AuthCookie, 5*time.Second)
	entries, err := c.RewriteList(context.Background())
	if err != nil {
		t.Fatalf("RewriteList: %v", err)
	}
	if logins != 1 {
		t.Fatalf("应恰好登录 1 次, got %d", logins)
	}
	if len(entries) != 1 || entries[0].Domain != "a.example.com" || entries[0].Answer != "104.16.1.1" {
		t.Fatalf("rewrite 解析不符: %+v", entries)
	}
	if entries[0].Enabled == nil || !*entries[0].Enabled {
		t.Fatalf("enabled 应为 true")
	}
}

func TestCookieSessionExpiredReLogin(t *testing.T) {
	valid := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/login":
			http.SetCookie(w, &http.Cookie{Name: "agh_session", Value: "sess2"})
		case "/control/querylog":
			if !valid {
				valid = true // 首次：模拟会话过期
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"oldest":"","data":[]}`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "pass", AuthCookie, 5*time.Second)
	if _, err := c.QueryLogPage(context.Background(), QueryLogParams{Limit: 10}); err != nil {
		t.Fatalf("会话过期后应重登重试成功: %v", err)
	}
}

func TestAuthFailureNoRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "bad", AuthBasic, 5*time.Second)
	_, err := c.QueryLogPage(context.Background(), QueryLogParams{})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("应返回 ErrAuth: %v", err)
	}
	if calls != 1 {
		t.Fatalf("认证失败不得重试, 调用 %d 次", calls)
	}

	c2 := New(srv.URL, "admin", "bad", AuthCookie, 5*time.Second)
	_, err = c2.RewriteList(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("cookie 模式登录失败应返回 ErrAuth: %v", err)
	}
}

func TestRewriteListLegacyNoEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"domain":"old.example.com","answer":"104.16.1.9"}]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "u", "p", AuthBasic, 5*time.Second)
	entries, err := c.RewriteList(context.Background())
	if err != nil {
		t.Fatalf("RewriteList: %v", err)
	}
	if len(entries) != 1 || entries[0].Enabled != nil {
		t.Fatalf("旧版条目应容忍缺失 enabled: %+v", entries)
	}
}

func TestServerErrorWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := New(srv.URL, "u", "p", AuthBasic, 5*time.Second)
	_, err := c.RewriteList(context.Background())
	if err == nil || errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "500") {
		t.Fatalf("5xx 应包装状态码: %v", err)
	}
}
