package detector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// cfIPsEndpoint Cloudflare 官方 IP 段端点（docs/API.md §5）。
const cfIPsEndpoint = "https://api.cloudflare.com/client/v4/ips"

// cidrCacheTTL 缓存新鲜期；过期缓存仅在拉取失败时兜底使用。
const cidrCacheTTL = 7 * 24 * time.Hour

// cfIPsResponse 官方端点响应（仅取所需字段）。
type cfIPsResponse struct {
	Result struct {
		IPv4CIDRs []string `json:"ipv4_cidrs"`
		IPv6CIDRs []string `json:"ipv6_cidrs"`
	} `json:"result"`
}

// cidrCache 本地缓存文件结构。
type cidrCache struct {
	FetchedAt time.Time `json:"fetched_at"`
	CIDRs     []string  `json:"cidrs"`
}

// SetCIDRs 注入 CIDR 列表（测试直塞；生产走 LoadCFIPs）。
func (d *Detector) SetCIDRs(nets []*net.IPNet) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cidrs = nets
}

// LoadCFIPs 获取 CF 官方 CIDR：新鲜缓存直接用 → 拉取并写缓存 →
// 拉取失败回退旧缓存（无论过期）→ 两者皆无返回 nil（CIDR 信号降级缺失，不单独决定结论）。
// cachePath 为空时不落盘。versions 仅支持 4 / 6。
func LoadCFIPs(ctx context.Context, hc *http.Client, cachePath string, versions []int, log *slog.Logger) ([]*net.IPNet, error) {
	if log == nil {
		log = slog.Default()
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	wantV4, wantV6 := false, false
	for _, v := range versions {
		switch v {
		case 4:
			wantV4 = true
		case 6:
			wantV6 = true
		}
	}

	if cachePath != "" {
		if nets, fresh := readCIDRCache(cachePath, wantV4, wantV6); fresh {
			log.Info("CF IP 段使用新鲜缓存", "path", cachePath, "count", len(nets))
			return nets, nil
		}
	}

	nets, err := fetchCFIPs(ctx, hc, wantV4, wantV6)
	if err != nil {
		if cachePath != "" {
			// 注意：此处新变量须与外层 nets 区分，避免遮蔽造成回退值误用。
			if cachedNets, _ := readCIDRCache(cachePath, wantV4, wantV6); len(cachedNets) > 0 {
				log.Warn("CF IP 段拉取失败，回退本地缓存（可能过期）", "err", err)
				return cachedNets, nil
			}
		}
		return nil, err
	}

	if cachePath != "" {
		if err := writeCIDRCache(cachePath, nets); err != nil {
			log.Warn("CF IP 段缓存写入失败", "path", cachePath, "err", err)
		}
	}
	log.Info("CF IP 段拉取成功", "count", len(nets))
	return nets, nil
}

// fetchCFIPs 从官方端点拉取并按版本过滤。
func fetchCFIPs(ctx context.Context, hc *http.Client, wantV4, wantV6 bool) ([]*net.IPNet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfIPsEndpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("官方端点 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var parsed cfIPsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	var raws []string
	if wantV4 {
		raws = append(raws, parsed.Result.IPv4CIDRs...)
	}
	if wantV6 {
		raws = append(raws, parsed.Result.IPv6CIDRs...)
	}
	nets, err := parseCIDRs(raws)
	if err != nil {
		return nil, err
	}
	if len(nets) == 0 {
		return nil, fmt.Errorf("官方端点未返回任何 CIDR")
	}
	return nets, nil
}

func parseCIDRs(raws []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(raws))
	for _, raw := range raws {
		_, n, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("非法 CIDR %q: %w", raw, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// readCIDRCache 读缓存；返回 fresh=是否在 7 天新鲜期内。
func readCIDRCache(path string, wantV4, wantV6 bool) ([]*net.IPNet, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var c cidrCache
	if json.Unmarshal(data, &c) != nil || len(c.CIDRs) == 0 {
		return nil, false
	}
	nets, err := parseCIDRs(c.CIDRs)
	if err != nil {
		return nil, false
	}
	// 缓存按当前所需版本重过滤（上次运行版本配置可能不同）。
	filtered := nets[:0:0]
	for _, n := range nets {
		isV4 := n.IP.To4() != nil
		if (isV4 && wantV4) || (!isV4 && wantV6) {
			filtered = append(filtered, n)
		}
	}
	if len(filtered) == 0 {
		return nil, false
	}
	return filtered, time.Since(c.FetchedAt) < cidrCacheTTL
}

func writeCIDRCache(path string, nets []*net.IPNet) error {
	raws := make([]string, len(nets))
	for i, n := range nets {
		raws[i] = n.String()
	}
	data, err := json.Marshal(cidrCache{FetchedAt: time.Now().UTC(), CIDRs: raws})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
