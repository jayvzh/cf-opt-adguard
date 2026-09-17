// Package state SQLite 状态库：迁移、容量护栏（D15）与 CRUD。
// 查询日志原文永不入库；domains 上限 1000 行、runs 只留最近 60 次。
package state

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// 容量护栏默认值（D15；DATA_MODEL 容量护栏节）。
const (
	MaxDomains = 1000
	MaxRuns    = 60
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB 状态库句柄。并发安全由 database/sql 连接池保证。
type DB struct {
	db  *sql.DB
	log *slog.Logger
}

// Open 打开（或创建）状态库，应用未执行的迁移，并做启动护栏清理。
func Open(path string, log *slog.Logger) (*DB, error) {
	if log == nil {
		log = slog.Default()
	}
	d, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开状态库失败: %w", err)
	}
	// modernc/sqlite 单写者：限制为单连接串行写，避免 SQLITE_BUSY。
	d.SetMaxOpenConns(1)
	if err := d.Ping(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("连接状态库失败: %w", err)
	}
	s := &DB{db: d, log: log}
	if err := s.migrate(); err != nil {
		_ = d.Close()
		return nil, err
	}
	if n := s.trimRuns(context.Background()); n > 0 {
		log.Warn("runs 超限清理", "removed", n, "limit", MaxRuns)
	}
	return s, nil
}

// Close 关闭底层连接。
func (s *DB) Close() error { return s.db.Close() }

// migrate 按序应用 migrations/*.sql 中未执行的迁移（只增不改）。
func (s *DB) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("创建 schema_migrations 失败: %w", err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("读取迁移目录失败: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		var done int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version,
		).Scan(&done); err != nil {
			return fmt.Errorf("查询迁移版本失败: %w", err)
		}
		if done > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("读取迁移 %s 失败: %w", name, err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("应用迁移 %s 失败: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, nowUTC(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("记录迁移 %s 失败: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.log.Info("迁移已应用", "version", name)
	}
	return nil
}

// migrationVersion 从文件名提取数字前缀（0001_init.sql → 1）。
func migrationVersion(name string) (int64, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("迁移文件名 %s 缺少数字前缀", name)
	}
	v, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("迁移文件名 %s 前缀非法: %w", name, err)
	}
	return v, nil
}

// EnforceCaps 容量护栏（D15）：runs>60 删最旧；domains>1000 按 last_seen 淘汰。
// 返回删除总行数；调用点：Open（runs）与每次运行落库后（domains/runs）。
func (s *DB) EnforceCaps(ctx context.Context) (removed int64, err error) {
	if n := s.trimRuns(ctx); n > 0 {
		s.log.Warn("runs 超限清理", "removed", n, "limit", MaxRuns)
		removed += n
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM domains WHERE id NOT IN (
		SELECT id FROM domains ORDER BY last_seen DESC, id DESC LIMIT ?)`, MaxDomains)
	if err != nil {
		return removed, fmt.Errorf("domains 护栏清理失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.log.Warn("domains 超限淘汰（按 last_seen 最旧）", "removed", n, "limit", MaxDomains)
		removed += n
	}
	return removed, nil
}

func (s *DB) trimRuns(ctx context.Context) int64 {
	res, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id NOT IN (
		SELECT id FROM runs ORDER BY started_at DESC, id DESC LIMIT ?)`, MaxRuns)
	if err != nil {
		s.log.Warn("runs 护栏清理失败", "err", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func formatUTC(t time.Time) string { return t.UTC().Format(time.RFC3339) }
