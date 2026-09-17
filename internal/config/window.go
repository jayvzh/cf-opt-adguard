package config

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// dayWeekRe 支持 Nd / Nw 形态（24h/7d/30d）。
var dayWeekRe = regexp.MustCompile(`^(\d+)(d|w)$`)

// ParseWindow 解析采集窗口：标准 Go duration（如 12h）之外支持 d（天）/ w（周）后缀。
func ParseWindow(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("窗口必须为正: %s", s)
		}
		return d, nil
	}
	m := dayWeekRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("窗口格式非法（支持 24h / 7d / 30d / 2w 等）: %s", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("窗口数值非法: %s", s)
	}
	switch m[2] {
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	default: // w
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
}
