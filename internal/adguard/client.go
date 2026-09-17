// Package adguard 提供 AdGuard Home HTTP 客户端（querylog / rewrite list / login，
// 以及 rewrite add/update/delete 写端点封装）。
// 契约唯一来源 docs/API.md；写方法仅可由 internal/syncer 调用（ARCHITECTURE §4.6 唯一写侧）。
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
	"sync/atomic"
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

	legacyReason atomic.Bool // 实例为旧版枚举拼写（降级后置位，进程内记住）
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
		Host string `json:"host"` // 新版（0.107.32+）域名字段（IDN 解码后）
		Name string `json:"name"` // 旧版域名字段（zone file 格式），两版均有
		Type string `json:"type"`
	} `json:"question"`
	Reason string `json:"reason"`
	Client string `json:"client"`
	Time   string `json:"time"`
}

// HostOrName 查询域名：新版 host 优先，空则回退旧版 name（pitfalls.md #9）。
func (e QueryLogEntry) HostOrName() string {
	if e.Question.Host != "" {
		return e.Question.Host
	}
	return e.Question.Name
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

// reason 枚举随 AGH 版本改名（0.107.x 前后 NotFilteredWhiteList → NotFilteredAllowList），
// 新值在旧实例上返回 400 实测见 pitfalls.md #8。
const (
	reasonAllowNew = "NotFilteredAllowList"
	reasonAllowOld = "NotFilteredWhiteList"
)

// downgradeReasons 新枚举替换为旧拼写（其余值两版通用）。
func downgradeReasons(rs []string) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		if r == reasonAllowNew {
			out[i] = reasonAllowOld
		} else {
			out[i] = r
		}
	}
	return out
}

// isReasonEnumErr 服务端 400 且报文中提及新枚举值 → 旧版实例不认识该枚举。
func isReasonEnumErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "返回 400") && strings.Contains(s, reasonAllowNew)
}

// QueryLogPage 拉取一页查询日志。reason 枚举跨版本兼容：旧实例不认新值时自动
// 降级旧拼写重试一次并记住，后续翻页直接使用旧值，避免逐页多打一次失败请求。
func (c *Client) QueryLogPage(ctx context.Context, p QueryLogParams) (*QueryLogPage, error) {
	if c.legacyReason.Load() {
		p.Reasons = downgradeReasons(p.Reasons)
	}
	page, err := c.queryLogOnce(ctx, p)
	if !isReasonEnumErr(err) {
		return page, err
	}
	c.legacyReason.Store(true)
	p.Reasons = downgradeReasons(p.Reasons)
	return c.queryLogOnce(ctx, p)
}

func (c *Client) queryLogOnce(ctx context.Context, p QueryLogParams) (*QueryLogPage, error) {
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

// rewritePayload 写端点请求体的单条目形态（API.md §3；enabled 不主动下发，
// 由 AGH 默认处理，避免误启用用户手动停用的条目）。
type rewritePayload struct {
	Domain string `json:"domain"`
	Answer string `json:"answer"`
}

// RewriteAdd 新增重写条目（写端点，仅 syncer 调用）。
func (c *Client) RewriteAdd(ctx context.Context, domain, answer string) error {
	return c.doJSON(ctx, http.MethodPost, "/control/rewrite/add", nil,
		rewritePayload{Domain: domain, Answer: answer}, nil)
}

// RewriteUpdate 按 target 精确修改单条（写端点，仅 syncer 调用）。
func (c *Client) RewriteUpdate(ctx context.Context, domain, oldAnswer, newAnswer string) error {
	body := map[string]rewritePayload{
		"target": {Domain: domain, Answer: oldAnswer},
		"update": {Domain: domain, Answer: newAnswer},
	}
	return c.doJSON(ctx, http.MethodPost, "/control/rewrite/update", nil, body, nil)
}

// RewriteDelete 删除单条（写端点，仅 syncer 调用）。
func (c *Client) RewriteDelete(ctx context.Context, domain, answer string) error {
	return c.doJSON(ctx, http.MethodPost, "/control/rewrite/delete", nil,
		rewritePayload{Domain: domain, Answer: answer}, nil)
}

// doJSON 执行请求并解析 JSON 响应；cookie 模式遇 401（会话过期）允许重登一次后重放原请求
//（写请求 401 意味着未被执行，重放安全；403 认证失败不重试，API.md §4）。
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
