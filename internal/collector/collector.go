// Package collector 负责从 AGH 查询日志翻页采集窗口内记录，并做类型 / 客户端 / 域名过滤。
// 翻页契约见 docs/API.md §2.1（P0 仅新版参数：limit + older_than 向前 + reason 多值，D11）。
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/aggregate"
	"cf-opt-adguard/internal/config"
)

// reasons P0 采集范围（D11：正常放行记录；服务端过滤）。
var reasons = []string{"NotFilteredNotFound", "NotFilteredAllowList"}

// StoppedReason 翻页终止原因。
const (
	StopEOF      = "eof"       // 服务端无更多记录
	StopWindow   = "window"    // 已越过窗口起点
	StopMaxPages = "max_pages" // 达到翻页上限（数据可能不完整）
	StopTimeout  = "timeout"   // 达到采集总超时（数据可能不完整）
)

// Stats 采集统计（打印进计划，便于核对覆盖范围）。
type Stats struct {
	Pages   int
	Fetched int    // 服务端返回条数
	Kept    int    // 过滤后保留条数
	Stopped string // StoppedReason 之一
}

// Collector 查询日志采集器。
type Collector struct {
	client *adguard.Client
	window time.Duration
	cfg    config.QueryLogConfig
	log    *slog.Logger
}

func New(client *adguard.Client, window time.Duration, cfg config.QueryLogConfig, log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	return &Collector{client: client, window: window, cfg: cfg, log: log}
}

// Collect 翻页拉取窗口内记录并过滤。返回按时间倒序的原始页序条目（聚合顺序无关）。
func (c *Collector) Collect(ctx context.Context, filter *aggregate.DomainFilter) ([]aggregate.Entry, Stats, error) {
	deadline := time.Now().Add(c.cfg.FetchTimeout)
	windowStart := time.Now().Add(-c.window)

	var out []aggregate.Entry
	st := Stats{Stopped: StopEOF}
	older := time.Time{} // zero = 首页

	for {
		if err := ctx.Err(); err != nil {
			return nil, st, fmt.Errorf("采集被取消: %w", err)
		}
		if !time.Now().Before(deadline) {
			st.Stopped = StopTimeout
			c.log.Warn("采集达到总超时，结果可能不完整", "fetch_timeout", c.cfg.FetchTimeout)
			break
		}
		if st.Pages >= c.cfg.MaxPages {
			st.Stopped = StopMaxPages
			c.log.Warn("达到翻页上限，结果可能不完整", "max_pages", c.cfg.MaxPages)
			break
		}

		page, err := c.client.QueryLogPage(ctx, adguard.QueryLogParams{
			Limit:     c.cfg.PageSize,
			OlderThan: older,
			Reasons:   reasons,
		})
		if err != nil {
			return nil, st, fmt.Errorf("拉取查询日志（第 %d 页）: %w", st.Pages+1, err)
		}
		st.Pages++
		if len(page.Entries) == 0 {
			break
		}

		earliest := time.Time{}
		for _, e := range page.Entries {
			t, err := time.Parse(time.RFC3339Nano, e.Time)
			if err != nil {
				c.log.Warn("跳过无法解析时间的记录", "time", e.Time)
				continue
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
			st.Fetched++
			if t.Before(windowStart) {
				continue // 窗口外仅计数
			}
			if e.Question.Type != "A" && e.Question.Type != "AAAA" {
				continue // 只消费 A / AAAA（API.md §2.2）
			}
			host := aggregate.Normalize(e.Question.Host)
			if host == "" {
				continue
			}
			if !c.clientAllowed(e.Client) {
				continue
			}
			if filter != nil && !filter.Allow(host) {
				continue
			}
			out = append(out, aggregate.Entry{Host: host, Client: e.Client, Time: e.Time})
			st.Kept++
		}

		if !earliest.IsZero() && earliest.Before(windowStart) {
			st.Stopped = StopWindow
			break
		}
		if earliest.IsZero() {
			break // 全页时间非法，无法继续向前
		}
		older = earliest // 向前翻页：从本页最早一条继续
	}
	return out, st, nil
}

// clientAllowed 客户端过滤：include 非空时命中才留；否则命中 exclude 即丢（精确匹配）。
func (c *Collector) clientAllowed(client string) bool {
	if len(c.cfg.ClientsInclude) > 0 {
		for _, in := range c.cfg.ClientsInclude {
			if client == in {
				return true
			}
		}
		return false
	}
	for _, ex := range c.cfg.ClientsExclude {
		if client == ex {
			return false
		}
	}
	return true
}
