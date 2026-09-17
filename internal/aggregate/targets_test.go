package aggregate

import (
	"reflect"
	"testing"
)

func cands(hosts ...string) []Candidate {
	out := make([]Candidate, len(hosts))
	for i, h := range hosts {
		out[i] = Candidate{Host: h, Hits: 100}
	}
	return out
}

func domains(ts []Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Domain
	}
	return out
}

func TestZoneOf(t *testing.T) {
	if got := ZoneOf("a.b.example.com"); got != "example.com" {
		t.Fatalf("ZoneOf 子域 = %s", got)
	}
	if got := ZoneOf("example.com"); got != "example.com" {
		t.Fatalf("裸域应返回自身, got %s", got)
	}
	if got := ZoneOf("www.example.co.uk"); got != "example.co.uk" {
		t.Fatalf("PSL 多级后缀 = %s", got)
	}
}

func TestResolveTargetsExact(t *testing.T) {
	c := cands("a.example.com", "b.example.com", "c.example.org")
	v := map[string]Verdict{
		"a.example.com": VerdictConfirmed,
		"b.example.com": VerdictNotCF,
		"c.example.org": VerdictConfirmed,
	}
	ts := ResolveTargets(c, v, TargetsOptions{Mode: "exact", AllowWildcard: true})
	want := []string{"a.example.com", "c.example.org"} // not_cf 剔除
	if !reflect.DeepEqual(domains(ts), want) {
		t.Fatalf("exact 目标不符: %v", domains(ts))
	}
	if ts[0].Zone != "a.example.com" || !reflect.DeepEqual(ts[0].Hosts, []string{"a.example.com"}) {
		t.Fatalf("exact 条目形态不符: %+v", ts[0])
	}
}

func TestResolveTargetsZoneFullWildcard(t *testing.T) {
	c := cands("a.example.com", "b.example.com", "example.com")
	v := map[string]Verdict{
		"a.example.com": VerdictConfirmed,
		"b.example.com": VerdictConfirmed,
		"example.com":   VerdictConfirmed,
	}
	ts := ResolveTargets(c, v, TargetsOptions{Mode: "zone", AllowWildcard: true})
	want := []string{"*.example.com", "example.com"}
	if !reflect.DeepEqual(domains(ts), want) {
		t.Fatalf("应生成裸域+通配两条: %v", domains(ts))
	}
	for _, x := range ts {
		if x.Zone != "example.com" || len(x.Hosts) != 3 {
			t.Fatalf("通配条目应覆盖 zone 全部 host: %+v", x)
		}
	}
}

func TestResolveTargetsZoneBareOnly(t *testing.T) {
	// "仅一级域套 CDN"：zone 内仅裸域达阈且 confirmed → 单条裸域。
	c := cands("example.com")
	v := map[string]Verdict{"example.com": VerdictConfirmed}
	ts := ResolveTargets(c, v, TargetsOptions{Mode: "zone", AllowWildcard: true})
	want := []string{"example.com"}
	if !reflect.DeepEqual(domains(ts), want) {
		t.Fatalf("仅裸域应单条: %v", domains(ts))
	}
	if ts[0].Wildcard {
		t.Fatal("裸域条目不应带通配")
	}
}

func TestResolveTargetsZoneSingleSubdomain(t *testing.T) {
	// zone 内仅一个 confirmed 子域：通配会波及未观测子域（如 mx），必须单条精确。
	c := cands("cdn.example.org")
	v := map[string]Verdict{"cdn.example.org": VerdictConfirmed}
	ts := ResolveTargets(c, v, TargetsOptions{Mode: "zone", AllowWildcard: true})
	want := []string{"cdn.example.org"}
	if !reflect.DeepEqual(domains(ts), want) {
		t.Fatalf("单子域应精确单条: %v", domains(ts))
	}
}

func TestResolveTargetsZoneFallbackMixed(t *testing.T) {
	c := cands("a.example.com", "b.example.com", "lan.example.com", "other.example.org")
	v := map[string]Verdict{
		"a.example.com":     VerdictConfirmed,
		"b.example.com":     VerdictConfirmed,
		"lan.example.com":   VerdictNotCF, // 内网服务混入同 zone → 禁止通配
		"other.example.org": VerdictConfirmed,
	}
	ts := ResolveTargets(c, v, TargetsOptions{Mode: "zone", AllowWildcard: true})
	want := []string{"a.example.com", "b.example.com", "other.example.org"}
	if !reflect.DeepEqual(domains(ts), want) {
		t.Fatalf("混入非 CF 应回退逐 host 精确: %v", domains(ts))
	}
	for _, x := range ts {
		if x.Wildcard {
			t.Fatalf("回退时不应出现通配: %+v", x)
		}
	}
}

func TestResolveTargetsZoneFallbackMaybe(t *testing.T) {
	// maybe 证据不足，同样触发回退，且 maybe host 不产生条目。
	c := cands("a.example.com", "m.example.com")
	v := map[string]Verdict{
		"a.example.com": VerdictConfirmed,
		"m.example.com": VerdictMaybe,
	}
	ts := ResolveTargets(c, v, TargetsOptions{Mode: "zone", AllowWildcard: true})
	want := []string{"a.example.com"}
	if !reflect.DeepEqual(domains(ts), want) {
		t.Fatalf("maybe 应触发回退且不产出条目: %v", domains(ts))
	}
}

func TestResolveTargetsNoWildcard(t *testing.T) {
	c := cands("a.example.com", "b.example.com")
	v := map[string]Verdict{"a.example.com": VerdictConfirmed, "b.example.com": VerdictConfirmed}
	ts := ResolveTargets(c, v, TargetsOptions{Mode: "zone", AllowWildcard: false})
	want := []string{"a.example.com", "b.example.com"}
	if !reflect.DeepEqual(domains(ts), want) {
		t.Fatalf("禁用通配应全精确: %v", domains(ts))
	}
}

func TestResolveTargetsMissingVerdict(t *testing.T) {
	// verdict 缺失按 not_cf 保守处理：不产出条目。
	c := cands("a.example.com")
	ts := ResolveTargets(c, nil, TargetsOptions{Mode: "zone", AllowWildcard: true})
	if len(ts) != 0 {
		t.Fatalf("无 verdict 不应产出条目: %v", ts)
	}
}

func TestAggregateCountsAndCap(t *testing.T) {
	entries := []Entry{
		{Host: "a.com"}, {Host: "a.com"}, {Host: "a.com"},
		{Host: "b.com"}, {Host: "b.com"},
		{Host: "c.com"}, // 低于阈值
	}
	got := Aggregate(entries, 2, 10)
	if len(got) != 2 || got[0].Host != "a.com" || got[0].Hits != 3 || got[1].Host != "b.com" {
		t.Fatalf("聚合结果不符: %+v", got)
	}
	// maxDomains 截断
	got = Aggregate(entries, 2, 1)
	if len(got) != 1 || got[0].Host != "a.com" {
		t.Fatalf("应按频次截断 top1: %+v", got)
	}
}
