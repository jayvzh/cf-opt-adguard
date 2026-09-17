package aggregate

import (
	"sort"

	"golang.org/x/net/publicsuffix"
)

// Verdict 探测结论（与 state/domains.confidence 同义；缺失按 not_cf 保守处理）。
type Verdict string

const (
	VerdictConfirmed Verdict = "confirmed"
	VerdictMaybe     Verdict = "maybe"
	VerdictNotCF     Verdict = "not_cf"
)

// Target 一条待写入的 rewrite 目标（Domain 为 rewrite 条目域名；Wildcard 表示 *. 前缀通配）。
type Target struct {
	Domain   string
	Wildcard bool
	Zone     string   // 归并域（exact 模式 = 自身）
	Hosts    []string // 该目标覆盖的源 host（审计展示用，已排序）
}

// ZoneOf 返回 host 的可注册域（PSL 归并）；无法归并时返回自身。
func ZoneOf(host string) string {
	z, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || z == "" {
		return host
	}
	return z
}

// TargetsOptions 归并决策参数。
type TargetsOptions struct {
	Mode          string // "zone"（D14 默认）| "exact"
	AllowWildcard bool   // sync.wildcard
}

// ResolveTargets 从达阈候选 + 探测结论生成 rewrite 目标清单（D14 混合决策）。
//
// zone 模式按可注册域分组后逐 zone 决策：
//   - zone 内仅 1 个达阈 host → 单条精确（裸域或子域，证据不足以通配）；
//   - zone 内 ≥2 个达阈 host 全部 confirmed 且允许通配 → zone + *.zone 两条；
//   - 混入 maybe / not_cf 达阈 host（或禁用通配）→ 该 zone 回退逐 host 精确。
//
// verdict 缺失按 not_cf 保守处理。输出按 Domain 字典序稳定排序。
func ResolveTargets(cands []Candidate, verdicts map[string]Verdict, opts TargetsOptions) []Target {
	confirmedOnly := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if verdicts[c.Host] == VerdictConfirmed {
			confirmedOnly = append(confirmedOnly, c)
		}
	}
	if opts.Mode == "exact" {
		return exactTargets(confirmedOnly)
	}
	return zoneTargets(confirmedOnly, cands, verdicts, opts)
}

func exactTargets(cands []Candidate) []Target {
	out := make([]Target, 0, len(cands))
	for _, c := range cands {
		out = append(out, Target{
			Domain: c.Host,
			Zone:   c.Host,
			Hosts:  []string{c.Host},
		})
	}
	sortTargets(out)
	return out
}

func zoneTargets(confirmed, all []Candidate, verdicts map[string]Verdict, opts TargetsOptions) []Target {
	// 按 zone 分组：confirmed 与"同 zone 的全部达阈 host"分别收集。
	type zoneInfo struct {
		confirmed []string
		all       []string
	}
	zones := map[string]*zoneInfo{}
	for _, c := range all {
		z := ZoneOf(c.Host)
		zi := zones[z]
		if zi == nil {
			zi = &zoneInfo{}
			zones[z] = zi
		}
		zi.all = append(zi.all, c.Host)
	}
	for _, c := range confirmed {
		z := ZoneOf(c.Host)
		zones[z].confirmed = append(zones[z].confirmed, c.Host)
	}

	out := make([]Target, 0, len(zones)*2)
	for z, zi := range zones {
		sort.Strings(zi.confirmed)
		sort.Strings(zi.all)
		if len(zi.confirmed) == 0 {
			continue
		}
		switch {
		case len(zi.all) == 1:
			// zone 内仅一个达阈 host：证据不足以断言整个 zone 归属，单条精确
			// （裸域单条 = "仅一级域套 CDN" 场景；单子域单条，避免通配误伤未观测子域如 mx）。
			h := zi.all[0]
			out = append(out, Target{Domain: h, Zone: z, Hosts: []string{h}})
		case len(zi.all) >= 2 && allConfirmed(zi.all, verdicts) && opts.AllowWildcard:
			// zone 内 ≥2 个达阈 host 全部 confirmed：裸域 + 通配两条（D14 归并主路径）。
			out = append(out,
				Target{Domain: z, Zone: z, Hosts: append([]string(nil), zi.all...)},
				Target{Domain: "*." + z, Wildcard: true, Zone: z, Hosts: append([]string(nil), zi.all...)},
			)
		default:
			// 混入 maybe / not_cf（或禁用通配）：回退逐 host 精确，绝不误伤。
			for _, h := range zi.confirmed {
				out = append(out, Target{Domain: h, Zone: z, Hosts: []string{h}})
			}
		}
	}
	sortTargets(out)
	return out
}

func allConfirmed(hosts []string, verdicts map[string]Verdict) bool {
	for _, h := range hosts {
		if verdicts[h] != VerdictConfirmed {
			return false
		}
	}
	return true
}

func sortTargets(ts []Target) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Domain < ts[j].Domain })
}
