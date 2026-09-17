package planner

import (
	"fmt"
	"net"
	"testing"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/aggregate"
)

var testIP = net.ParseIP("104.21.88.54")

func agh(domain, answer string) adguard.RewriteEntry {
	return adguard.RewriteEntry{Domain: domain, Answer: answer}
}

func target(domain string) aggregate.Target {
	return aggregate.Target{Domain: domain, Zone: domain, Hosts: []string{domain}}
}

// keys 计划条目的 "Action domain→answer" 摘要，便于断言整体形态。
func keys(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, fmt.Sprintf("%s %s→%s", e.Action, e.Domain, e.Answer))
	}
	return out
}

func equalStrs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("条数不符: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项不符:\n got  %v\n want %v", i, got, want)
		}
	}
}

func TestDiffEmptyStateFirstRun(t *testing.T) {
	targets := []aggregate.Target{
		target("example.com"),
		{Domain: "*.example.com", Wildcard: true, Zone: "example.com", Hosts: []string{"example.com", "a.example.com"}},
		target("cdn.foo.net"),
	}
	plan := Diff(targets, nil, map[string]bool{}, testIP, 4)
	// 输出顺序 = 输入顺序（ResolveTargets 产出的 targets 已按 Domain 排序）
	equalStrs(t, keys(plan.Add), []string{
		"add example.com→104.21.88.54",
		"add *.example.com→104.21.88.54",
		"add cdn.foo.net→104.21.88.54",
	})
	if len(plan.Update)+len(plan.Remove) != 0 {
		t.Fatalf("首跑不应有 update/remove: %+v", plan)
	}
	if plan.Count(ActionAdd) != 3 || plan.Count(ActionUpdate) != 0 || plan.Count(ActionRemove) != 0 || plan.Empty() {
		t.Fatal("计数/Empty 判断错误")
	}
}

func TestDiffNoopWhenConsistent(t *testing.T) {
	aghList := []adguard.RewriteEntry{agh("example.com", "104.21.88.54")}
	plan := Diff([]aggregate.Target{target("example.com")}, aghList, map[string]bool{"example.com": true}, testIP, 4)
	if !plan.Empty() {
		t.Fatalf("一致时应为空计划: %+v", plan)
	}
}

func TestDiffUpdateWhenAnswerDiffers(t *testing.T) {
	aghList := []adguard.RewriteEntry{agh("example.com", "1.0.0.1")}
	managed := map[string]bool{"example.com": true}
	plan := Diff([]aggregate.Target{target("example.com")}, aghList, managed, testIP, 4)
	equalStrs(t, keys(plan.Update), []string{"update example.com→104.21.88.54"})
	// update 场景同时产出旧答案清理（同族）
	equalStrs(t, keys(plan.Remove), []string{"remove example.com→1.0.0.1"})
	if plan.Update[0].Reason == "" {
		t.Fatal("update 应携带替换原因")
	}
}

func TestDiffSkipsUnmanaged(t *testing.T) {
	// 用户手工条目：不在托管集合 → 既不 remove，也不影响 add 判定之外的行为
	aghList := []adguard.RewriteEntry{agh("manual.lan", "192.168.1.1")}
	plan := Diff([]aggregate.Target{target("example.com")}, aghList, map[string]bool{}, testIP, 4)
	equalStrs(t, keys(plan.Add), []string{"add example.com→104.21.88.54"})
	if len(plan.Remove) != 0 {
		t.Fatalf("托管集合之外的条目绝不可触碰: %+v", plan.Remove)
	}
}

func TestDiffRemovesStaleManagedEntry(t *testing.T) {
	// 旧托管域名已不在本次目标集合 → AGH 条目进入 remove
	aghList := []adguard.RewriteEntry{agh("stale.com", "1.0.0.1")}
	managed := map[string]bool{"stale.com": true}
	plan := Diff([]aggregate.Target{target("example.com")}, aghList, managed, testIP, 4)
	equalStrs(t, keys(plan.Remove), []string{"remove stale.com→1.0.0.1"})
}

func TestDiffKeepsOtherFamilyAnswer(t *testing.T) {
	// 同托管域名的 v6 答案与本次 v4 期望不冲突 → 不清理
	aghList := []adguard.RewriteEntry{
		agh("example.com", "2606:4700::6815:5836"),
		agh("example.com", "1.0.0.1"), // 同族旧答案 → 清理
	}
	managed := map[string]bool{"example.com": true}
	plan := Diff([]aggregate.Target{target("example.com")}, aghList, managed, testIP, 4)
	equalStrs(t, keys(plan.Remove), []string{"remove example.com→1.0.0.1"})
}

func TestDiffNilPreferredIP(t *testing.T) {
	plan := Diff([]aggregate.Target{target("example.com")}, nil, nil, nil, 4)
	if !plan.Empty() {
		t.Fatalf("preferredIP 为 nil 应返回空计划: %+v", plan)
	}
}
