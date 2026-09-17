package aggregate

import (
	"sort"
)

// Candidate 达到阈值的候选域名。
type Candidate struct {
	Host string
	Hits int
}

// Aggregate 频次统计 → 阈值筛选 → 按频次降序截断 top maxDomains（D15 探测预算护栏）。
func Aggregate(entries []Entry, minHits, maxDomains int) []Candidate {
	counts := make(map[string]int, len(entries)/4)
	for _, e := range entries {
		counts[e.Host]++
	}
	cands := make([]Candidate, 0, len(counts))
	for host, hits := range counts {
		if hits >= minHits {
			cands = append(cands, Candidate{Host: host, Hits: hits})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Hits != cands[j].Hits {
			return cands[i].Hits > cands[j].Hits
		}
		return cands[i].Host < cands[j].Host // 同频按字典序，保证输出稳定
	})
	if maxDomains > 0 && len(cands) > maxDomains {
		cands = cands[:maxDomains]
	}
	return cands
}
