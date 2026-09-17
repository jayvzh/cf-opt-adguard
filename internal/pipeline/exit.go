package pipeline

import (
	"cf-opt-adguard/internal/aggregate"
	"cf-opt-adguard/internal/config"
)

// 退出码契约（docs/API.md §7.3）。4 为 live 同步部分失败，本阶段不触达。
const (
	ExitOK    = 0 // dry-run 正常产出计划
	ExitNoIP  = 2 // 配置 / 参数错误、优选 IP 为空（中止，不生成 remove 计划）
	ExitError = 3 // 采集 / 探测 / 落库等运行失败，未做任何写操作
)

// newDomainFilter 从配置构造域名黑白名单过滤器。
func newDomainFilter(cfg *config.Config) *aggregate.DomainFilter {
	return aggregate.NewDomainFilter(cfg.Domains.Include, cfg.Domains.Exclude)
}
