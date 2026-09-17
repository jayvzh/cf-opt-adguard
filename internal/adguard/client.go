// Package adguard 提供 AdGuard Home 只读 HTTP 客户端（querylog / rewrite list / login）。
// 契约唯一来源 docs/API.md；写接口（rewrite add/update/delete）由 syncer 在第二阶段实现，本包禁止新增写方法。
package adguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 认证模式（API.md §1.2：二选一，默认 Basic）。
const (
	AuthBasic  = "basic"
	AuthCookie = "cookie"
)

// ErrAuth 认证失败（401/403）。调用方收到它必须中止，禁止重试（API.md §1.2）。
var ErrAuth = errors.New("adguard 认证失败")

// Client AGH 只读客户端。并发安全。
type Client struct {
	baseURL  string
	hc       *http.Client
	username string
	password string
	authMode string

	mu     sync.Mutex
	cookie *http.Cookie
}

// New 创建客户端。authMode 为空取 AuthBasic。
func New(baseURL, username, password, authMode string, timeout time.Duration) *Client {
	if authMode == "" {
		authMode = AuthBasic
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		hc:       &http.Client{Timeout: timeout},
		username: username,
		password: password,
		authMode: authMode,
	}
}

// QueryLogEntry 查询日志单条（只定义消费字段，未知字段忽略，API.md §2.2）。
type QueryLogEntry struct {
	Question struct {
		Host string `json:"host"`
		Type string `json:"type"`
	} `json:"question"`
	Reason string `json:"reason"`
	Client string `json:"client"`
	Time   string `json:"time"`
}

// QueryLogPage querylog 单页响应。
type QueryLogPage struct {
	Oldest  string          `json:"oldest"`
	Entries []QueryLogEntry `json:"data"`
}

// QueryLogParams 单次翻页参数（P0 仅新版参数集，D11）。
type QueryLogParams struct {
	Limit     int
	OlderThan time.Time // zero = 首页（不传）
	Reasons   []string  // 可多次传参，如 NotFilteredNotFound
}

// QueryLogPage 拉取一页查询日志。
func (c *Client) QueryLogPage(ctx context.Context, p QueryLogParams) (*QueryLogPage, error) {
	q := url.Values{}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if !p.OlderThan.IsZero() {
		q.Set("older_than", p.OlderThan.UTC().Format(time.RFC3339Nano))
	}
	for _, r := range p.Reasons {
		q.Add("reason", r)
	}
	var page QueryLogPage
	if err := c.doJSON(ctx, http.MethodGet, "/control/querylog", q, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// RewriteEntry 重写条目（旧版无 enabled 字段 → 指针容忍，API.md §3）。
type RewriteEntry struct {
	Domain  string `json:"domain"`
	Answer  string `json:"answer"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// RewriteList 获取全部重写条目。
func (c *Client) RewriteList(ctx context.Context) ([]RewriteEntry, error) {
	var entries []RewriteEntry
	if err := c.doJSON(ctx, http.MethodGet, "/control/rewrite/list", nil, nil, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// doJSON 执行请求并解析 JSON 响应；cookie 模式遇会话过期允许重登一次重试（仅限本包只读方法，API.md §4）。
func (c *Client) doJSON(ctx context.Context, method, path string, q url.Values, body, out any) error {
	err := c.doOnce(ctx, method, path, q, body, out)
	if err == nil || !errors.Is(err, ErrAuth) || c.authMode != AuthCookie {
		return err
	}
	// 会话过期：清 cookie 重新登录，再试一次原请求。
	c.mu.Lock()
	c.cookie = nil
	c.mu.Unlock()
	return c.doOnce(ctx, method, path, q, body, out)
}

func (c *Client) doOnce(ctx context.Context, method, path string, q url.Values, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求体: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	switch c.authMode {
	case AuthBasic:
		req.SetBasicAuth(c.username, c.password)
	case AuthCookie:
		if err := c.ensureSession(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		if c.cookie != nil {
			req.AddCookie(c.cookie)
		}
		c.mu.Unlock()
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		io.Copy(io.Discard, resp.Body)
		return ErrAuth
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s 返回 %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("解析 %s 响应: %w", path, err)
	}
	return nil
}

// ensureSession cookie 模式下保证存在有效会话（POST /control/login，API.md §4）。
func (c *Client) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	if c.cookie != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	payload, err := json.Marshal(map[string]string{"name": c.username, "password": c.password})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/control/login", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("登录失败: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrAuth
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("登录返回 %d", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Value != "" {
			c.mu.Lock()
			c.cookie = ck
			c.mu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("登录成功但未返回会话 Cookie")
}
