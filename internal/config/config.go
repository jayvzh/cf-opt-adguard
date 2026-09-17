// Package config 负责配置加载：YAML 文件 + flags 覆盖 + ${ENV} 展开 + 默认值与必填校验。
// 默认值集中在此处定义，与 docs/DATA_MODEL.md §4 配置项全集保持同义；护栏默认值变化必须同步该文档。
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 全部配置项（键名与 docs/DATA_MODEL.md §4 一一对应）。
type Config struct {
	AdGuard   AdGuardConfig   `yaml:"adguard"`
	QueryLog  QueryLogConfig  `yaml:"querylog"`
	Aggregate AggregateConfig `yaml:"aggregate"`
	Domains   DomainsConfig   `yaml:"domains"`
	Detector  DetectorConfig  `yaml:"detector"`
	CFIP      CFIPConfig      `yaml:"cfip"`
	Sync      SyncConfig      `yaml:"sync"`
	Runtime   RuntimeConfig   `yaml:"runtime"`

	// WindowDur 由 Window 解析得出，Load 时填充。
	WindowDur time.Duration `yaml:"-"`
}

type AdGuardConfig struct {
	URL      string        `yaml:"url"`
	Username string        `yaml:"username"`
	Password string        `yaml:"password"` // 支持 ${ENV_VAR} 展开（D13）
	Timeout  time.Duration `yaml:"timeout"`
}

type QueryLogConfig struct {
	Window         string   `yaml:"window"` // 24h / 7d / 30d
	PageSize       int      `yaml:"page_size"`
	MaxPages       int      `yaml:"max_pages"`
	FetchTimeout   time.Duration `yaml:"fetch_timeout"`
	ClientsInclude []string `yaml:"clients_include"`
	ClientsExclude []string `yaml:"clients_exclude"`
}

type AggregateConfig struct {
	Mode       string `yaml:"mode"`         // zone（默认，D14）| exact
	MinHits24h int    `yaml:"min_hits_24h"` // 20
	MinHits7d  int    `yaml:"min_hits_7d"`  // 50
	MaxDomains int    `yaml:"max_domains"`  // 探测预算上限（D15 护栏）
}

type DomainsConfig struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

type DetectorConfig struct {
	Resolvers      []string      `yaml:"resolvers"` // 独立 resolver（IP[:port] 或 DoH URL 形态 P0 仅支持 ip[:port]）
	Concurrency    int           `yaml:"concurrency"`
	Timeout        time.Duration `yaml:"timeout"`
	HTTPEnabled    bool          `yaml:"http_enabled"`
	ScoreThreshold int           `yaml:"score_threshold"` // 4
}

type CFIPConfig struct {
	Source     string `yaml:"source"` // domain:HOST | cfst:PATH | static:PATH
	Strategy   string `yaml:"strategy"`
	IPVersions []int  `yaml:"ip_versions"` // 默认 [4]
}

type SyncConfig struct {
	Mode      string        `yaml:"mode"` // rewrite-api（唯一支持值）
	Wildcard  bool          `yaml:"wildcard"`
	TTL       time.Duration `yaml:"ttl"`
	RateLimit time.Duration `yaml:"rate_limit"`
	Retry     int           `yaml:"retry"`
}

type RuntimeConfig struct {
	DBPath   string `yaml:"db_path"`
	Apply    bool   `yaml:"apply"`
	LogLevel string `yaml:"log_level"`
	LogFile  string `yaml:"log_file"`
}

// Overrides 命令行 flags 覆盖值（nil = 未提供，沿用文件 / 默认值，D13）。
type Overrides struct {
	AghURL     *string
	AghUser    *string
	AghPass    *string
	Window     *string
	MinHits    *int
	CFIPSource *string
	Apply      *bool
	LogLevel   *string
	DBPath     *string
}

// Default 返回集中定义的默认值（护栏基准，改动须同步 docs/DATA_MODEL.md §4 与 docs/PRD.md §6）。
func Default() *Config {
	c := &Config{
		AdGuard: AdGuardConfig{Timeout: 10 * time.Second},
		QueryLog: QueryLogConfig{
			Window:       "7d",
			PageSize:     500,
			MaxPages:     200,
			FetchTimeout: 5 * time.Minute,
		},
		Aggregate: AggregateConfig{
			Mode:       "zone", // D14：一级域混合 + 自动回退
			MinHits24h: 20,
			MinHits7d:  50,
			MaxDomains: 1000, // D15
		},
		Detector: DetectorConfig{
			Concurrency:    8,
			Timeout:        3 * time.Second,
			HTTPEnabled:    true,
			ScoreThreshold: 4,
		},
		CFIP: CFIPConfig{
			Source:     "cfst:result.csv", // D17：相对工作目录，release 二进制放 CFST 发布目录执行
			Strategy:   "lowest_latency",
			IPVersions: []int{4}, // v6 需 CFST 另跑 ipv6 后显式开启
		},
		Sync: SyncConfig{
			Mode:      "rewrite-api",
			Wildcard:  true, // zone 模式允许生成 *.zone 条目（D14）
			TTL:       30 * 24 * time.Hour,
			RateLimit: 200 * time.Millisecond,
			Retry:     3,
		},
		Runtime: RuntimeConfig{
			DBPath:   "./data/state.db",
			Apply:    false, // D10：默认 dry-run
			LogLevel: "info",
		},
	}
	c.WindowDur = 7 * 24 * time.Hour // 与 QueryLog.Window 默认值一致，保证 Default() 自洽可直接 Validate
	return c
}

// Load 读取 YAML（path 为空则只用默认值）→ ${ENV} 展开 → flags 覆盖 → 校验。
func Load(path string, o Overrides) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件: %w", err)
		}
		if err := yaml.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
		}
	}
	if err := c.expandEnv(); err != nil {
		return nil, err
	}
	c.applyOverrides(o)
	if err := c.fillDerived(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// applyOverrides 仅覆盖显式提供的 flags（Overrides 为值类型，指针字段零值 nil 即"未提供"）。
func (c *Config) applyOverrides(o Overrides) {
	if o.AghURL != nil {
		c.AdGuard.URL = *o.AghURL
	}
	if o.AghUser != nil {
		c.AdGuard.Username = *o.AghUser
	}
	if o.AghPass != nil {
		c.AdGuard.Password = *o.AghPass
	}
	if o.Window != nil {
		c.QueryLog.Window = *o.Window
	}
	if o.MinHits != nil {
		c.Aggregate.MinHits24h = *o.MinHits
		c.Aggregate.MinHits7d = *o.MinHits
	}
	if o.CFIPSource != nil {
		c.CFIP.Source = *o.CFIPSource
	}
	if o.Apply != nil {
		c.Runtime.Apply = *o.Apply
	}
	if o.LogLevel != nil {
		c.Runtime.LogLevel = *o.LogLevel
	}
	if o.DBPath != nil {
		c.Runtime.DBPath = *o.DBPath
	}
}

// fillDerived 派生字段：窗口时长。
func (c *Config) fillDerived() error {
	d, err := ParseWindow(c.QueryLog.Window)
	if err != nil {
		return err
	}
	c.WindowDur = d
	return nil
}

// Validate 必填项与取值域校验（exit code 2 的来源）。
func (c *Config) Validate() error {
	if c.AdGuard.URL == "" {
		return fmt.Errorf("adguard.url 必填（配置文件或 --agh-url）")
	}
	if c.AdGuard.Username == "" {
		return fmt.Errorf("adguard.username 必填")
	}
	if c.AdGuard.Timeout <= 0 {
		return fmt.Errorf("adguard.timeout 必须为正")
	}
	if c.WindowDur <= 0 {
		return fmt.Errorf("querylog.window 非法: %s", c.QueryLog.Window)
	}
	switch c.Aggregate.Mode {
	case "zone", "exact":
	default:
		return fmt.Errorf("aggregate.mode 仅支持 zone|exact，当前: %s", c.Aggregate.Mode)
	}
	if c.Aggregate.MinHits24h <= 0 || c.Aggregate.MinHits7d <= 0 {
		return fmt.Errorf("aggregate.min_hits_24h / min_hits_7d 必须为正")
	}
	if len(c.Detector.Resolvers) == 0 {
		return fmt.Errorf("detector.resolvers 必填至少一个独立 resolver（不可指向本机 AGH）")
	}
	if c.Detector.Concurrency <= 0 || c.Detector.Timeout <= 0 {
		return fmt.Errorf("detector.concurrency / detector.timeout 必须为正")
	}
	if c.Detector.ScoreThreshold <= 0 {
		return fmt.Errorf("detector.score_threshold 必须为正")
	}
	if c.CFIP.Source == "" {
		return fmt.Errorf("cfip.source 必填（domain:HOST | cfst:PATH | static:PATH）")
	}
	for _, v := range c.CFIP.IPVersions {
		if v != 4 && v != 6 {
			return fmt.Errorf("cfip.ip_versions 仅支持 4|6，当前: %d", v)
		}
	}
	switch c.CFIP.Strategy {
	case "lowest_latency", "roundrobin":
	default:
		return fmt.Errorf("cfip.strategy 仅支持 lowest_latency|roundrobin，当前: %s", c.CFIP.Strategy)
	}
	if c.Sync.Mode != "rewrite-api" {
		return fmt.Errorf("sync.mode 仅支持 rewrite-api")
	}
	if c.Sync.TTL <= 0 {
		return fmt.Errorf("sync.ttl 必须为正")
	}
	if c.Runtime.DBPath == "" {
		return fmt.Errorf("runtime.db_path 必填")
	}
	return nil
}

// MinHits 按窗口选择生效阈值（ARCHITECTURE §4.2：24h≥20 或 7d≥50）。
func (c *Config) MinHits() int {
	if c.WindowDur <= 24*time.Hour {
		return c.Aggregate.MinHits24h
	}
	return c.Aggregate.MinHits7d
}

// envRefRe 匹配 ${VAR_NAME}。
var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv 仅对密码字段做 ${ENV_VAR} 展开（D13 最小隐私面；未定义变量展开为空并在校验期暴露）。
func (c *Config) expandEnv() error {
	c.AdGuard.Password = envRefRe.ReplaceAllStringFunc(c.AdGuard.Password, func(m string) string {
		name := envRefRe.FindStringSubmatch(m)[1]
		return os.Getenv(name)
	})
	return nil
}
