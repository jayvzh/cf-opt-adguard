// Package detector 用独立 DNS resolver + Cloudflare 官方 CIDR + HTTP 头三类信号判定域名是否为 CF 站点。
// 打分与结论规则见 docs/ARCHITECTURE.md §4.3（权重改动须同步该文档与 PRD）。
package detector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"cf-opt-adguard/internal/aggregate"
	"cf-opt-adguard/internal/config"
)

// maxCNAMEDepth CNAME 链跟随上限（防环）。
const maxCNAMEDepth = 5

// cfCNAMEZoneSuffixes CNAME 链命中判定后缀（ARCHITECTURE §4.3；cdn.cloudflare.net 被 .cloudflare.net 覆盖）。
var cfCNAMEZoneSuffixes = []string{".cloudflare.net", ".cloudflare.com"}

// Signal 三类探测信号。
type Signal struct {
	CNAMECF  bool // CNAME 链命中 CF 域后缀
	CIDR     bool // 最终 A/AAAA 落在官方 CIDR
	CFRay    bool // 响应含 cf-ray
	ServerCF bool // server: cloudflare
}

// Score 打分纯函数：CNAME+2 / CIDR+2 / cf-ray+3 / server+1。
func Score(s Signal) int {
	n := 0
	if s.CNAMECF {
		n += 2
	}
	if s.CIDR {
		n += 2
	}
	if s.CFRay {
		n += 3
	}
	if s.ServerCF {
		n += 1
	}
	return n
}

// Classify 依分数定结论：≥阈值 confirmed；有信号未达线 maybe；无信号 not_cf。
func Classify(score, threshold int) aggregate.Verdict {
	switch {
	case score >= threshold:
		return aggregate.VerdictConfirmed
	case score > 0:
		return aggregate.VerdictMaybe
	default:
		return aggregate.VerdictNotCF
	}
}

// Result 单域名探测结论（信号逐项保留，落库可追溯）。
type Result struct {
	Host    string
	Verdict aggregate.Verdict
	Score   int
	Signal  Signal
	Err     string // DNS 解析失败原因（此时 verdict 恒 not_cf）
}

// Detector 探测器。并发安全（CIDR 列表加载后只读）。
type Detector struct {
	resolvers   []string
	timeout     time.Duration
	httpEnabled bool
	threshold   int
	concurrency int
	hc          *http.Client
	log         *slog.Logger

	mu    sync.RWMutex
	cidrs []*net.IPNet
}

// New 创建探测器。hc 为 nil 时用默认客户端（生产）；测试可注入假 Transport。
func New(cfg config.DetectorConfig, hc *http.Client, log *slog.Logger) *Detector {
	if log == nil {
		log = slog.Default()
	}
	if hc == nil {
		hc = &http.Client{
			Timeout: cfg.Timeout,
			// 不跟随重定向：首响应头才是本域真实特征。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Detector{
		resolvers:   cfg.Resolvers,
		timeout:     cfg.Timeout,
		httpEnabled: cfg.HTTPEnabled,
		threshold:   cfg.ScoreThreshold,
		hc:          hc,
		log:         log,
		concurrency: cfg.Concurrency,
	}
}

// DetectAll 并发探测全部域名；结果顺序与输入一致。
func (d *Detector) DetectAll(ctx context.Context, hosts []string) []Result {
	out := make([]Result, len(hosts))
	sem := make(chan struct{}, d.concurrency)

	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = d.Detect(ctx, h)
		}(i, h)
	}
	wg.Wait()
	return out
}

// Detect 探测单域名（DNS 失败 → not_cf；HTTP 失败 → 信号缺失）。
func (d *Detector) Detect(ctx context.Context, host string) Result {
	res := Result{Host: host}
	ips, cnameCF, err := d.resolveChain(ctx, host)
	if err != nil {
		res.Err = err.Error()
		res.Verdict = aggregate.VerdictNotCF
		return res
	}
	sig := Signal{CNAMECF: cnameCF}
	for _, ip := range ips {
		if d.inCF(ip) {
			sig.CIDR = true
			break
		}
	}
	if d.httpEnabled {
		sig.CFRay, sig.ServerCF = d.probeHTTP(ctx, host)
	}
	res.Signal = sig
	res.Score = Score(sig)
	res.Verdict = Classify(res.Score, d.threshold)
	return res
}

// resolveChain 从独立 resolver 追 CNAME 链并收集最终 A 记录。
func (d *Detector) resolveChain(ctx context.Context, host string) ([]net.IP, bool, error) {
	var ips []net.IP
	cnameCF := false
	current := strings.ToLower(strings.TrimSuffix(host, "."))
	for hop := 0; hop < maxCNAMEDepth; hop++ {
		msg, err := d.exchange(ctx, current, dns.TypeA)
		if err != nil {
			return nil, false, err
		}
		gotIP := false
		next := ""
		for _, a := range msg.Answer {
			switch rr := a.(type) {
			case *dns.CNAME:
				t := strings.ToLower(strings.TrimSuffix(rr.Target, "."))
				if isCFCNAME(t) {
					cnameCF = true
				}
				if next == "" {
					next = t
				}
			case *dns.A:
				ips = append(ips, rr.A)
				gotIP = true
			}
		}
		if gotIP || next == "" {
			return ips, cnameCF, nil
		}
		current = next
	}
	return ips, cnameCF, nil
}

// exchange 依次尝试全部 resolver，取第一个成功应答（NOERROR）。
func (d *Detector) exchange(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.RecursionDesired = true
	cl := &dns.Client{Timeout: d.timeout}

	var lastErr error
	for _, r := range d.resolvers {
		in, _, err := cl.ExchangeContext(ctx, m, normalizeResolver(r))
		if err == nil && in.Rcode == dns.RcodeSuccess {
			return in, nil
		}
		if err == nil {
			lastErr = fmt.Errorf("响应码 %s", dns.RcodeToString[in.Rcode])
		} else {
			lastErr = err
		}
	}
	return nil, fmt.Errorf("解析 %s 失败: %w", name, lastErr)
}

// probeHTTP 探测 https://host 响应头；HEAD 失败降级 GET（ARCHITECTURE §4.3）。
func (d *Detector) probeHTTP(ctx context.Context, host string) (cfRay, serverCF bool) {
	do := func(method string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, "https://"+host+"/", nil)
		if err != nil {
			return nil, err
		}
		return d.hc.Do(req)
	}
	resp, err := do(http.MethodHead)
	if err != nil && resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		resp, err = do(http.MethodGet)
	}
	if err != nil {
		return false, false // 探测失败 = 信号缺失，不判死
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))

	cfRay = resp.Header.Get("Cf-Ray") != ""
	serverCF = strings.EqualFold(resp.Header.Get("Server"), "cloudflare")
	return cfRay, serverCF
}

func (d *Detector) inCF(ip net.IP) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, n := range d.cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func isCFCNAME(host string) bool {
	for _, suf := range cfCNAMEZoneSuffixes {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	return false
}

// normalizeResolver 无端口 resolver 补 53（IPv6 由 JoinHostPort 处理方括号）。
func normalizeResolver(r string) string {
	if _, _, err := net.SplitHostPort(r); err == nil {
		return r
	}
	return net.JoinHostPort(r, "53")
}
