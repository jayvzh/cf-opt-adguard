// Package planner 生成 rewrite 变更计划（纯函数，禁止任何 IO）。
// 归属判定见 docs/ARCHITECTURE.md §4.5：只以状态库 active/pending 域名集合为托管边界，
// 集合之外的用户手工 rewrite 绝不出现在计划中。
package planner

import (
	"fmt"
	"net"
	"strings"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/aggregate"
)

// Action 变更类型。
type Action string

const (
	ActionAdd    Action = "add"
	ActionUpdate Action = "update"
	ActionRemove Action = "remove"
)

// Entry 计划中的单条变更（dry-run 打印与 live syncer 消费同一份结构）。
type Entry struct {
	Action    Action
	Domain    string // 与 AGH 条目同形态；通配条目带 *. 前缀
	Answer    string // add/update = 优选 IP；remove = AGH 现有答案
	IPVersion int
	Reason    string
}

// Plan 一份不可变的变更计划。
type Plan struct {
	Add    []Entry
	Update []Entry
	Remove []Entry
}

// Count 按 Action 汇总条数。
func (p Plan) Count(a Action) int {
	switch a {
	case ActionAdd:
		return len(p.Add)
	case ActionUpdate:
		return len(p.Update)
	case ActionRemove:
		return len(p.Remove)
	}
	return 0
}

// Empty 计划是否无任何变更。
func (p Plan) Empty() bool {
	return len(p.Add) == 0 && len(p.Update) == 0 && len(p.Remove) == 0
}

// Diff 对比目标集合与 AGH 现状，产出 add / update / remove 三类操作。
//
//   - add：目标域名在 AGH 中不存在；
//   - update：目标域名存在但现有答案不含优选 IP（syncer 执行时替换）；
//   - remove：托管域名在 AGH 中的条目，答案与本次期望不符且同 IP 族（旧答案清理，
//     本次仅打印不执行）；异族答案（v4/v6 互补）不属于本次清理范围。
//
// preferredIP 为 nil 时返回空计划（上游优选失败已中止，防御性兜底）。
func Diff(targets []aggregate.Target, aghList []adguard.RewriteEntry, managed map[string]bool, preferredIP net.IP, ipVersion int) Plan {
	var plan Plan
	if preferredIP == nil {
		return plan
	}
	answer := preferredIP.String()

	want := make(map[string]string, len(targets)) // domain（含 *. 前缀形态）→ 期望 answer
	for _, t := range targets {
		want[t.Domain] = answer
	}
	have := make(map[string][]string) // domain → AGH 现有 answers
	for _, e := range aghList {
		have[e.Domain] = append(have[e.Domain], e.Answer)
	}

	// targets 已按 Domain 排序（ResolveTargets 保证），此处按序遍历保持计划输出稳定。
	for _, t := range targets {
		answers, exists := have[t.Domain]
		switch {
		case !exists:
			plan.Add = append(plan.Add, Entry{
				Action: ActionAdd, Domain: t.Domain, Answer: answer, IPVersion: ipVersion,
				Reason: "AGH 无此条目",
			})
		case !containsStr(answers, answer):
			plan.Update = append(plan.Update, Entry{
				Action: ActionUpdate, Domain: t.Domain, Answer: answer, IPVersion: ipVersion,
				Reason: fmt.Sprintf("替换现有答案 %s", strings.Join(answers, ",")),
			})
		}
		// 已与期望一致 → 无操作
	}

	// remove：仅触碰托管集合内的域名；AGH 条目答案与期望不符（同族）才清理。
	for _, e := range aghList {
		if !managed[e.Domain] || want[e.Domain] == e.Answer {
			continue
		}
		ver := ipVersionOf(e.Answer)
		if ver != ipVersion {
			continue // 异族答案互补共存，不在本次清理范围
		}
		plan.Remove = append(plan.Remove, Entry{
			Action: ActionRemove, Domain: e.Domain, Answer: e.Answer, IPVersion: ver,
			Reason: "托管域名答案与本次期望不符",
		})
	}
	return plan
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ipVersionOf 依答案文本判断 IP 族（含 ":" 即 v6）。
func ipVersionOf(answer string) int {
	if strings.Contains(answer, ":") {
		return 6
	}
	return 4
}
