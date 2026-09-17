package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.QueryLog.Window != "7d" || c.QueryLog.PageSize != 500 || c.QueryLog.MaxPages != 200 {
		t.Fatalf("querylog 默认值不符: %+v", c.QueryLog)
	}
	if c.Aggregate.Mode != "zone" || c.Aggregate.MinHits24h != 20 || c.Aggregate.MinHits7d != 50 || c.Aggregate.MaxDomains != 1000 {
		t.Fatalf("aggregate 默认值不符: %+v", c.Aggregate)
	}
	if c.Detector.ScoreThreshold != 4 || c.Detector.Concurrency != 8 || !c.Detector.HTTPEnabled {
		t.Fatalf("detector 默认值不符: %+v", c.Detector)
	}
	if len(c.CFIP.IPVersions) != 1 || c.CFIP.IPVersions[0] != 4 {
		t.Fatalf("cfip.ip_versions 默认值不符: %v", c.CFIP.IPVersions)
	}
	if !c.Sync.Wildcard || c.Sync.RateLimit != 200*time.Millisecond || c.Sync.Retry != 3 {
		t.Fatalf("sync 默认值不符: %+v", c.Sync)
	}
	if c.Runtime.Apply {
		t.Fatal("apply 默认必须为 false（D10 dry-run）")
	}
}

// 较小 YAML，只改必要键；其余走默认值。
const testYAML = `
adguard:
  url: http://192.168.1.2:3000
  username: admin
  password: ${CF_OPT_TEST_PASS}
querylog:
  window: 24h
detector:
  resolvers: ["223.5.5.5"]
cfip:
  source: cfst:/tmp/result.csv
`

func TestLoadFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(testYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CF_OPT_TEST_PASS", "s3cret")

	c, err := Load(path, Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AdGuard.URL != "http://192.168.1.2:3000" {
		t.Fatalf("url 不符: %s", c.AdGuard.URL)
	}
	if c.AdGuard.Password != "s3cret" {
		t.Fatalf("${ENV} 展开失败: %q", c.AdGuard.Password)
	}
	if c.WindowDur != 24*time.Hour {
		t.Fatalf("WindowDur 派生失败: %v", c.WindowDur)
	}
	if c.MinHits() != 20 { // 24h 窗口 → 24h 阈值
		t.Fatalf("MinHits 应取 24h 阈值 20, got %d", c.MinHits())
	}
	// 默认值兜底仍生效
	if c.QueryLog.PageSize != 500 || c.Runtime.DBPath != "./data/state.db" {
		t.Fatalf("默认值未兜底: %+v", c.Runtime)
	}
}

func TestLoadOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(testYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	url := "http://10.0.0.1:3000"
	win := "2w"
	minHits := 100
	apply := true
	o := Overrides{AghURL: &url, Window: &win, MinHits: &minHits, Apply: &apply}

	c, err := Load(path, o)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AdGuard.URL != url {
		t.Fatalf("flags 覆盖 url 失败: %s", c.AdGuard.URL)
	}
	if c.WindowDur != 14*24*time.Hour {
		t.Fatalf("flags 覆盖 window 失败: %v", c.WindowDur)
	}
	if c.Aggregate.MinHits24h != 100 || c.Aggregate.MinHits7d != 100 {
		t.Fatalf("flags 覆盖 min_hits 应同步两个阈值")
	}
	if c.MinHits() != 100 {
		t.Fatalf("2w 窗口 MinHits 应为 100, got %d", c.MinHits())
	}
	if !c.Runtime.Apply {
		t.Fatal("flags 覆盖 apply 失败")
	}
}

func TestLoadEnvUndefined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(testYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("CF_OPT_TEST_PASS") // 未定义 → 展开为空，但 Load 不报错（密码非必填校验项）
	c, err := Load(path, Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AdGuard.Password != "" {
		t.Fatalf("未定义变量应展开为空: %q", c.AdGuard.Password)
	}
}

func TestValidateRequired(t *testing.T) {
	// Validate 为 fail-fast：逐项补全，确认每个必填项都有独立报错。
	c := Default()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "adguard.url") {
		t.Fatalf("缺 url 应报错: %v", err)
	}
	c.AdGuard.URL = "http://x"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("缺 username 应报错: %v", err)
	}
	c.AdGuard.Username = "admin"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "resolvers") {
		t.Fatalf("缺 resolvers 应报错: %v", err)
	}
	c.Detector.Resolvers = []string{"223.5.5.5"}
	// D17：cfip.source 有默认值 cfst:result.csv，仅显式置空才触发护栏。
	c.CFIP.Source = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cfip.source") {
		t.Fatalf("置空 cfip.source 应报错: %v", err)
	}
	c.CFIP.Source = "cfst:result.csv"
	c.WindowDur = 7 * 24 * time.Hour
	if err := c.Validate(); err != nil {
		t.Fatalf("补全后应通过: %v", err)
	}
}

func TestValidateEnum(t *testing.T) {
	base := func() *Config {
		c := Default()
		c.AdGuard.URL = "http://x"
		c.AdGuard.Username = "admin"
		c.Detector.Resolvers = []string{"223.5.5.5"}
		c.CFIP.Source = "static:/tmp/ips.txt"
		c.WindowDur = 24 * time.Hour
		return c
	}
	c := base()
	c.Aggregate.Mode = "bogus"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "zone|exact") {
		t.Fatalf("mode 枚举校验失败: %v", err)
	}
	c = base()
	c.CFIP.IPVersions = []int{5}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "4|6") {
		t.Fatalf("ip_versions 校验失败: %v", err)
	}
	c = base()
	c.Sync.Mode = "file"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "rewrite-api") {
		t.Fatalf("sync.mode 校验失败: %v", err)
	}
}

func TestParseWindow(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"24h", 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"0d", 0, true},
		{"-5m", 0, true},
		{"7x", 0, true},
		{"", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseWindow(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseWindow(%q) 应报错, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseWindow(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseWindow(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLoadFileMissing(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml", Overrides{})
	if err == nil {
		t.Fatal("缺失文件应报错")
	}
	// path 为空 → 纯默认值，仅因必填缺失而失败
	_, err = Load("", Overrides{})
	if err == nil || !strings.Contains(err.Error(), "adguard.url") {
		t.Fatalf("纯默认应因 url 必填失败: %v", err)
	}
}
