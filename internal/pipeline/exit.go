package pipeline

import (
	"cf-opt-adguard/internal/aggregate"
	"cf-opt-adguard/internal/config"
)

// 退出码契约（docs/API.md §7.3）。
const (
	ExitOK        = 0 // dry-run 正常产出计划 / live 同步全部成功
	ExitNoIP      = 2 // 配置 / 参数错误、优选 IP 为空（中止，不生成 remove 计划）
	ExitError     = 3 // 采集 / 探测 / 落库等运行失败
	ExitPartial   = 4 // live 同步或回查验证部分失败（其余条目已生效）
	ExitCancelled = 5 // live 写入前用户确认取消（计划已产出，未执行任何写操作）
)

// newDomainFilter 从配置构造域名黑白名单过滤器。
func newDomainFilter(cfg *config.Config) *aggregate.DomainFilter {
	return aggregate.NewDomainFilter(cfg.Domains.Include, cfg.Domains.Exclude)
}
