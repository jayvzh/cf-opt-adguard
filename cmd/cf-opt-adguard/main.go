// cf-opt-adguard：AdGuard Home 的 Cloudflare rewrite 外部编排器。
// 默认 dry-run（只读 + 计划打印）；--apply 进入 live 模式，
// 由 syncer 作为唯一写侧执行计划并经 AGH DNS 回查验证。
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/config"
	"cf-opt-adguard/internal/pipeline"
	"cf-opt-adguard/internal/state"
)

// version 可由 -ldflags "-X main.version=..." 注入。
var version = "0.1.0-dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return pipeline.ExitNoIP
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "export":
		return cmdExport(args[1:])
	case "version", "-v", "--version":
		fmt.Printf("cf-opt-adguard %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return pipeline.ExitOK
	case "help", "-h", "--help":
		usage(os.Stdout)
		return pipeline.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", args[0])
		usage(os.Stderr)
		return pipeline.ExitNoIP
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `cf-opt-adguard — AdGuard Home 的 Cloudflare rewrite 外部编排器

用法:
  cf-opt-adguard run     [flags]   执行一次完整流水线（默认 dry-run，只读 + 计划打印）
  cf-opt-adguard export  [flags]   从状态库导出 domains.csv / rewrites.csv
  cf-opt-adguard version           打印版本
  cf-opt-adguard help              本帮助

dry-run 绝不调用 AdGuard Home 写端点；--apply 为 live 写入（syncer 唯一写侧，
部分失败退出码 4），写入后经 AGH DNS 回查验证。
`)
}

// ptr 指针化 flag 覆盖值（nil = 未提供，沿用配置文件 / 默认值，D13）。
func ptr[T any](v T) *T { return &v }

func cmdRun(args []string) int {
	fs := newFlagSet("run")
	cfgPath := fs.String("c", "", "配置文件路径（YAML，可选）")
	aghURL := fs.String("agh-url", "", "覆盖 adguard.url")
	aghUser := fs.String("agh-user", "", "覆盖 adguard.username")
	aghPass := fs.String("agh-pass", "", "覆盖 adguard.password")
	window := fs.String("window", "", "覆盖 querylog.window（如 24h / 7d / 30d）")
	minHits := fs.Int("min-hits", 0, "覆盖聚合命中阈值（同时作用于 24h 与 7d 档）")
	cfipSource := fs.String("cfip-source", "", "覆盖 cfip.source（默认 cfst:result.csv，D17）")
	apply := fs.Bool("apply", false, "进入 live 模式：执行计划写入 AGH 并回查验证（默认 dry-run）")
	logLevel := fs.String("log-level", "", "覆盖 runtime.log_level")
	dbPath := fs.String("db-path", "", "覆盖 runtime.db_path")
	if err := fs.Parse(args); err != nil {
		return pipeline.ExitNoIP
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "run 不接受位置参数: %v\n", fs.Args())
		return pipeline.ExitNoIP
	}

	cfg, err := config.Load(*cfgPath, config.Overrides{
		AghURL:     optStr(*aghURL),
		AghUser:    optStr(*aghUser),
		AghPass:    optStr(*aghPass),
		Window:     optStr(*window),
		MinHits:    optInt(*minHits),
		CFIPSource: optStr(*cfipSource),
		Apply:      optBool(*apply),
		LogLevel:   optStr(*logLevel),
		DBPath:     optStr(*dbPath),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		return pipeline.ExitNoIP
	}

	log := newLogger(cfg.Runtime.LogLevel, cfg.Runtime.LogFile)
	if cfg.Runtime.Apply {
		log.Warn("live 模式：将执行计划写入 AdGuard Home（syncer 唯一写侧）")
	}
	// 确保状态库目录存在（首次运行 ./data 不存在时 sqlite 无法自行建目录）。
	if dir := filepath.Dir(cfg.Runtime.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "创建数据目录失败:", err)
			return pipeline.ExitError
		}
	}
	client := adguard.New(cfg.AdGuard.URL, cfg.AdGuard.Username, cfg.AdGuard.Password, "", cfg.AdGuard.Timeout)
	db, err := state.Open(cfg.Runtime.DBPath, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开状态库失败:", err)
		return pipeline.ExitError
	}
	defer func() { _ = db.Close() }()

	_, code, err := pipeline.Run(context.Background(), cfg, pipeline.Deps{
		Client: client,
		DB:     db,
		Log:    log,
		Stdout: os.Stdout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "运行失败（退出码 %d）: %v\n", code, err)
		if code == pipeline.ExitNoIP {
			fmt.Fprintln(os.Stderr, "提示: 请先在 CloudflareSpeedTest 目录运行测速生成 result.csv，或将本工具二进制放入 CFST 发布目录执行（D17）")
		}
		return code
	}
	return code
}

func cmdExport(args []string) int {
	fs := newFlagSet("export")
	cfgPath := fs.String("c", "", "配置文件路径（YAML）")
	dbPath := fs.String("db-path", "", "覆盖 runtime.db_path（默认 ./data/state.db）")
	outDir := fs.String("o", ".", "输出目录")
	if err := fs.Parse(args); err != nil {
		return pipeline.ExitNoIP
	}
	cfg, err := config.Load(*cfgPath, config.Overrides{DBPath: optStr(*dbPath)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		return pipeline.ExitNoIP
	}

	db, err := state.Open(cfg.Runtime.DBPath, slog.Default())
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开状态库失败:", err)
		return pipeline.ExitError
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if err := exportAll(ctx, db, *outDir); err != nil {
		fmt.Fprintln(os.Stderr, "导出失败:", err)
		return pipeline.ExitError
	}
	fmt.Printf("已导出 %s/domains.csv 与 %s/rewrites.csv\n", *outDir, *outDir)
	return pipeline.ExitOK
}

// exportAll 状态库只读导出两份 CSV。
func exportAll(ctx context.Context, db *state.DB, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	domains, err := db.ListDomains(ctx)
	if err != nil {
		return err
	}
	if err := writeCSV(filepath.Join(outDir, "domains.csv"), []string{
		"domain", "zone", "hit_count", "first_seen", "last_seen",
	}, len(domains), func(i int) []string {
		d := domains[i]
		return []string{d.Domain, d.Zone, strconv.Itoa(d.HitCount), fmtTime(d.FirstSeen), fmtTime(d.LastSeen)}
	}); err != nil {
		return err
	}

	var rewrites []state.Rewrite
	for _, st := range []string{"active", "pending", "removed"} {
		rs, err := db.ListRewritesByState(ctx, st)
		if err != nil {
			return err
		}
		rewrites = append(rewrites, rs...)
	}
	return writeCSV(filepath.Join(outDir, "rewrites.csv"), []string{
		"domain", "answer", "ip_version", "state", "agh_present",
		"first_synced", "last_synced", "last_error", "updated_at",
	}, len(rewrites), func(i int) []string {
		r := rewrites[i]
		return []string{
			r.Domain, r.Answer, strconv.Itoa(r.IPVersion), r.State,
			strconv.FormatBool(r.AGHPresent),
			fmtTime(r.FirstSynced), fmtTime(r.LastSynced), r.LastError, fmtTime(r.UpdatedAt),
		}
	})
}

// writeCSV 通用行迭代写出（header + rows(i)）。
func writeCSV(path string, header []string, n int, row func(i int) []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if err := w.Write(row(i)); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// fmtTime RFC3339 UTC；零值输出空串（导出可读性）。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// newFlagSet 统一错误输出到 stderr（flag 默认混用 os.Stderr 但显式更清晰）。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// optStr 空 → nil（未提供）。
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optInt(i int) *int {
	if i == 0 {
		return nil
	}
	return &i
}

func optBool(b bool) *bool {
	if !b {
		return nil // 仅显式 --apply 才覆盖；false 沿用配置（默认即 false）
	}
	return &b
}

// newLogger 按配置构建 slog（级别非法回退 info；文件打开失败回退 stderr）。
func newLogger(level, file string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(strings.ToLower(level))); err != nil {
		lv = slog.LevelInfo
	}
	var w io.Writer = os.Stderr
	if file != "" {
		if f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			w = f
		}
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv}))
}
