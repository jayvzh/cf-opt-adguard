// Package aggregate 负责 DNS 查询条目的归一化、频次聚合与域名归并决策（纯函数，禁止 IO）。
package aggregate

import (
	"strings"
)

// Entry 采集产物（collector 输出、频次聚合输入）。
type Entry struct {
	Host   string
	Client string
	Time   string // RFC3339（保持字符串避免重复格式化；聚合时按需解析）
}

// Normalize 域名归一化：小写 + 去根点 + 折叠连续点。空串视为无效。
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", ".")
	}
	return s
}

type filterRule struct {
	exact  string
	base   string // "*.corp" 的 "corp"（通配含裸域，过滤语义而非 DNS 语义）
	suffix string // ".corp"
}

// DomainFilter 域名黑白名单（配置 domains.include/exclude；include 优先）。
type DomainFilter struct {
	include []filterRule
	exclude []filterRule
}

// NewDomainFilter 构建过滤器；规则与 host 一律先 Normalize。nil 表示不过滤。
func NewDomainFilter(include, exclude []string) *DomainFilter {
	f := &DomainFilter{}
	for _, r := range include {
		if r = Normalize(r); r != "" {
			f.include = append(f.include, compileRule(r))
		}
	}
	for _, r := range exclude {
		if r = Normalize(r); r != "" {
			f.exclude = append(f.exclude, compileRule(r))
		}
	}
	return f
}

func compileRule(r string) filterRule {
	if strings.HasPrefix(r, "*.") {
		base := r[2:]
		return filterRule{base: base, suffix: "." + base}
	}
	return filterRule{exact: r}
}

// Allow 判定 host 是否保留：include 非空时命中才留；否则命中 exclude 即丢。
func (f *DomainFilter) Allow(host string) bool {
	host = Normalize(host)
	if len(f.include) > 0 {
		for _, r := range f.include {
			if match(r, host) {
				return true
			}
		}
		return false
	}
	for _, r := range f.exclude {
		if match(r, host) {
			return false
		}
	}
	return true
}

func match(r filterRule, host string) bool {
	if r.exact != "" {
		return host == r.exact
	}
	return host == r.base || strings.HasSuffix(host, r.suffix)
}
