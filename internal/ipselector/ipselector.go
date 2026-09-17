// Package ipselector 按配置来源产出候选优选 IP 列表（有序：最优在前）。
// 三来源（docs/ARCHITECTURE.md §4.4，D17 默认 B）：cfst:PATH / domain:HOST / static:PATH。
// 结果为空返回 ErrNoIP，由 pipeline 映射为退出码 2 并中止（不生成任何计划）。
package ipselector

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// ErrNoIP 优选 IP 结果为空（文件缺失 / 只有表头 / 全被版本过滤 / 域名解析失败）。
var ErrNoIP = errors.New("优选 IP 为空")

// errHint 引导用户的统一提示（附在 ErrNoIP 错误链上）。
const errHint = "请先在 CloudflareSpeedTest 目录运行测速生成 result.csv，或改配 cfip.source"

// Options Select 入参（全部来自 config.CFIPConfig + config.DetectorConfig.Resolvers）。
type Options struct {
	Source     string   // cfst:PATH | domain:HOST | static:PATH
	Strategy   string   // lowest_latency | roundrobin（仅 domain 来源有效）
	IPVersions []int    // 4 / 6 过滤
	Resolvers  []string // domain 来源的独立 resolver
	Timeout    time.Duration
}

// Select 按来源解析并按 ip_versions 过滤；输出顺序 = 最优在前。
func Select(ctx context.Context, opts Options, log *slog.Logger) ([]net.IP, error) {
	if log == nil {
		log = slog.Default()
	}
	kind, arg, ok := strings.Cut(opts.Source, ":")
	if !ok || arg == "" {
		return nil, fmt.Errorf("非法 cfip.source %q（应为 domain:HOST | cfst:PATH | static:PATH）", opts.Source)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	var (
		ips []net.IP
		err error
	)
	switch kind {
	case "cfst":
		ips, err = fromCFST(arg)
	case "static":
		ips, err = fromStatic(arg)
	case "domain":
		ips, err = fromDomain(ctx, arg, opts)
	default:
		return nil, fmt.Errorf("未知 cfip.source 前缀 %q（支持 cfst|domain|static）", kind)
	}
	if err != nil {
		return nil, err
	}

	ips = filterVersions(ips, opts.IPVersions)
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: %s（source=%s）", ErrNoIP, errHint, opts.Source)
	}
	log.Info("优选 IP 就绪", "count", len(ips), "first", ips[0].String(), "source", opts.Source)
	return ips, nil
}

// First 取列表首 IP（planner 默认下发答案）。
func First(ips []net.IP) net.IP {
	if len(ips) == 0 {
		return nil
	}
	return ips[0]
}

// filterVersions 按 4/6 过滤并保持原顺序。
func filterVersions(ips []net.IP, versions []int) []net.IP {
	wantV4, wantV6 := false, false
	for _, v := range versions {
		switch v {
		case 4:
			wantV4 = true
		case 6:
			wantV6 = true
		}
	}
	if !wantV4 && !wantV6 {
		return ips
	}
	out := ips[:0:0]
	for _, ip := range ips {
		isV4 := ip.To4() != nil
		if (isV4 && wantV4) || (!isV4 && wantV6) {
			out = append(out, ip)
		}
	}
	return out
}

// fromCFST 解析 CFST result.csv：encoding/csv 逐行、第 0 列为 IP、跳过表头与非法行。
// CSV 已按丢包/延迟、下载速度排序，顺序即优先级。
func fromCFST(path string) ([]net.IP, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取 %s 失败: %v；%s", ErrNoIP, path, err, errHint)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // 列数宽松，仅取第 0 列
	var ips []net.IP
	line := 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			continue // 单行格式错误跳过
		}
		line++
		if line == 1 {
			continue // 表头（中文，API.md §6）
		}
		if ip := net.ParseIP(strings.TrimSpace(rec[0])); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

// fromStatic 逐行解析纯 IP 文件；空行与 # 注释跳过。
func fromStatic(path string) ([]net.IP, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取 %s 失败: %v", ErrNoIP, path, err)
	}
	var ips []net.IP
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if ip := net.ParseIP(ln); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

// fromDomain 用独立 resolver 解析优选域名；多 IP 时按 strategy 排序
// （lowest_latency：TCP 443 握手计时升序；roundrobin：保持解析顺序）。
func fromDomain(ctx context.Context, host string, opts Options) ([]net.IP, error) {
	resolvers := opts.Resolvers
	if len(resolvers) == 0 {
		return nil, fmt.Errorf("domain 来源需要 detector.resolvers")
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeA)
	m.RecursionDesired = true
	cl := &dns.Client{Timeout: opts.Timeout}

	var ips []net.IP
	var lastErr error
	for _, r := range resolvers {
		addr := r
		if _, _, err := net.SplitHostPort(r); err != nil {
			addr = net.JoinHostPort(r, "53")
		}
		in, _, err := cl.ExchangeContext(ctx, m, addr)
		if err != nil {
			lastErr = err
			continue
		}
		if in.Rcode != dns.RcodeSuccess {
			lastErr = fmt.Errorf("响应码 %s", dns.RcodeToString[in.Rcode])
			continue
		}
		for _, a := range in.Answer {
			if rr, ok := a.(*dns.A); ok {
				ips = append(ips, rr.A)
			}
		}
		if len(ips) > 0 {
			break
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: 解析 %s 失败: %v", ErrNoIP, host, lastErr)
	}
	if opts.Strategy == "roundrobin" {
		return ips, nil
	}
	sortByTCP443(ips, "443", opts.Timeout)
	return ips, nil
}

// sortByTCP443 对 TCP 握手计时升序稳定排序；全部失败保持原序（port 可测注入）。
func sortByTCP443(ips []net.IP, port string, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	type scored struct {
		ip net.IP
		d  time.Duration
	}
	ss := make([]scored, len(ips))
	for i, ip := range ips {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), timeout)
		ss[i] = scored{ip: ip, d: time.Since(start)}
		if err == nil {
			_ = conn.Close()
		} else {
			ss[i].d = time.Hour // 探测失败沉底
		}
	}
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].d < ss[j].d })
	for i := range ss {
		ips[i] = ss[i].ip
	}
}
