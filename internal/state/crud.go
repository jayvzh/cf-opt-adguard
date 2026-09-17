package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"cf-opt-adguard/internal/aggregate"
)

// Domain domains 表行（不含自增 id 与审计列，由 upsert 管理）。
type Domain struct {
	Domain   string
	Zone     string
	HitCount int
	FirstSeen time.Time
	LastSeen  time.Time
}

// Probe domain_probes 表行。
type Probe struct {
	Domain      string
	CNAMEHit    bool
	CIDRHit     bool
	CFRayHit    bool
	ServerHit   bool
	Score       int
	Confidence  aggregate.Verdict
	CNAMEChain  string
	ResolvedIPs []net.IP
	Error       string
	ProbedAt    time.Time
}

// Rewrite rewrites 表行（PK domain+answer+ip_version）。
type Rewrite struct {
	Domain      string
	Answer      string
	IPVersion   int
	State       string // active | pending | removed
	AGHPresent  bool
	FirstSynced time.Time // 零值 → NULL
	LastSynced  time.Time
	LastError   string
	UpdatedAt   time.Time
}

// Run runs 表行。计数列用指针区分"未设置"（进行中）与 0。
type Run struct {
	ID          int64
	Mode        string // dry | live
	StartedAt   time.Time
	FinishedAt  time.Time
	LogEntries  *int
	Candidates  *int
	Confirmed   *int
	NAdd        int
	NUpdate     int
	NRemove     int
	NFailed     int
	PreferredIP string
	Note        string
}

// UpsertDomains 批量写入聚合结果：hit_count/zone/last_seen 每次重算，
// first_seen/created_at 保留首次值。
func (s *DB) UpsertDomains(ctx context.Context, domains []Domain) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := nowUTC()
	for _, d := range domains {
		if _, err := tx.ExecContext(ctx, `INSERT INTO domains
			(domain, zone, hit_count, first_seen, last_seen, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(domain) DO UPDATE SET
				zone = excluded.zone,
				hit_count = excluded.hit_count,
				last_seen = excluded.last_seen,
				updated_at = excluded.updated_at`,
			d.Domain, d.Zone, d.HitCount, formatUTC(d.FirstSeen), formatUTC(d.LastSeen), now, now,
		); err != nil {
			return fmt.Errorf("upsert domain %s 失败: %w", d.Domain, err)
		}
	}
	return tx.Commit()
}

// ListDomains 按命中频次降序列出全部聚合域名（export 导出用，只读）。
func (s *DB) ListDomains(ctx context.Context) ([]Domain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT domain, zone, hit_count, first_seen, last_seen
		 FROM domains ORDER BY hit_count DESC, domain`)
	if err != nil {
		return nil, fmt.Errorf("列出 domains: %w", err)
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		var d Domain
		var firstSeen, lastSeen string
		if err := rows.Scan(&d.Domain, &d.Zone, &d.HitCount, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		d.FirstSeen = parseUTC(firstSeen)
		d.LastSeen = parseUTC(lastSeen)
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpsertProbes 批量写入探测结论（逐信号 + 分数 + verdict）。
func (s *DB) UpsertProbes(ctx context.Context, probes []Probe) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range probes {
		ipsJSON := "[]"
		if len(p.ResolvedIPs) > 0 {
			strs := make([]string, len(p.ResolvedIPs))
			for i, ip := range p.ResolvedIPs {
				strs[i] = ip.String()
			}
			b, err := json.Marshal(strs)
			if err != nil {
				return err
			}
			ipsJSON = string(b)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO domain_probes
			(domain, cname_hit, cidr_hit, cf_ray_hit, server_hit, score, confidence,
			 cname_chain, resolved_ips, error, probed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(domain) DO UPDATE SET
				cname_hit = excluded.cname_hit, cidr_hit = excluded.cidr_hit,
				cf_ray_hit = excluded.cf_ray_hit, server_hit = excluded.server_hit,
				score = excluded.score, confidence = excluded.confidence,
				cname_chain = excluded.cname_chain, resolved_ips = excluded.resolved_ips,
				error = excluded.error, probed_at = excluded.probed_at`,
			p.Domain, b2i(p.CNAMEHit), b2i(p.CIDRHit), b2i(p.CFRayHit), b2i(p.ServerHit),
			p.Score, string(p.Confidence), p.CNAMEChain, ipsJSON, p.Error, formatUTC(p.ProbedAt),
		); err != nil {
			return fmt.Errorf("upsert probe %s 失败: %w", p.Domain, err)
		}
	}
	return tx.Commit()
}

// UpsertRewrite 写入/更新托管 rewrite 状态行。
func (s *DB) UpsertRewrite(ctx context.Context, r Rewrite) error {
	var firstSynced, lastSynced any
	if !r.FirstSynced.IsZero() {
		firstSynced = formatUTC(r.FirstSynced)
	}
	if !r.LastSynced.IsZero() {
		lastSynced = formatUTC(r.LastSynced)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO rewrites
		(domain, answer, ip_version, state, agh_present, first_synced, last_synced, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(domain, answer, ip_version) DO UPDATE SET
			state = excluded.state, agh_present = excluded.agh_present,
			last_synced = excluded.last_synced, last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		r.Domain, r.Answer, r.IPVersion, r.State, b2i(r.AGHPresent),
		firstSynced, lastSynced, r.LastError, formatUTC(r.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert rewrite %s 失败: %w", r.Domain, err)
	}
	return nil
}

// SetRewriteState 只更新状态列（状态机流转用）。
func (s *DB) SetRewriteState(ctx context.Context, domain, answer string, ipVersion int, state string, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rewrites
		SET state = ?, last_error = ?, updated_at = ?
		WHERE domain = ? AND answer = ? AND ip_version = ?`,
		state, lastErr, nowUTC(), domain, answer, ipVersion,
	)
	return err
}

// ListManaged 托管归属集合（ARCH §4.5）：state ∈ {active, pending} 的去重域名。
// 集合之外的用户手工 rewrite 一律不碰。
func (s *DB) ListManaged(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT domain FROM rewrites WHERE state IN ('active','pending')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out[d] = true
	}
	return out, rows.Err()
}

// ListRewritesByState 按状态列出 rewrite 行（pipeline/planner 消费）。
func (s *DB) ListRewritesByState(ctx context.Context, state string) ([]Rewrite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		domain, answer, ip_version, state, agh_present, first_synced, last_synced, last_error, updated_at
		FROM rewrites WHERE state = ? ORDER BY domain, answer`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rewrite
	for rows.Next() {
		r, err := scanRewrite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanRewrite(r rowScanner) (Rewrite, error) {
	var rw Rewrite
	var aghPresent int
	var firstSynced, lastSynced, lastError sql.NullString
	var updatedAt string
	if err := r.Scan(&rw.Domain, &rw.Answer, &rw.IPVersion, &rw.State, &aghPresent,
		&firstSynced, &lastSynced, &lastError, &updatedAt); err != nil {
		return rw, err
	}
	rw.AGHPresent = aghPresent != 0
	rw.LastError = lastError.String
	if firstSynced.Valid {
		rw.FirstSynced = parseUTC(firstSynced.String)
	}
	if lastSynced.Valid {
		rw.LastSynced = parseUTC(lastSynced.String)
	}
	rw.UpdatedAt = parseUTC(updatedAt)
	return rw, nil
}

// InsertRun 开始一次运行审计，返回 run id。
func (s *DB) InsertRun(ctx context.Context, r Run) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs (mode, started_at) VALUES (?, ?)`,
		r.Mode, formatUTC(r.StartedAt))
	if err != nil {
		return 0, fmt.Errorf("insert run 失败: %w", err)
	}
	return res.LastInsertId()
}

// FinishRun 补全运行结果（各计数 + 优选 IP + 备注）。
func (s *DB) FinishRun(ctx context.Context, id int64, r Run) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET
		finished_at = ?, log_entries = ?, candidates = ?, confirmed = ?,
		n_add = ?, n_update = ?, n_remove = ?, n_failed = ?,
		preferred_ip = ?, note = ?
		WHERE id = ?`,
		formatUTC(r.FinishedAt), r.LogEntries, r.Candidates, r.Confirmed,
		r.NAdd, r.NUpdate, r.NRemove, r.NFailed,
		r.PreferredIP, r.Note, id,
	)
	if err != nil {
		return fmt.Errorf("finish run %d 失败: %w", id, err)
	}
	return nil
}

// ErrNotFound ListRewrite 查无此行。
var ErrNotFound = errors.New("状态库无此行")

// GetRewrite 单条查询（幂等判断用）。
func (s *DB) GetRewrite(ctx context.Context, domain, answer string, ipVersion int) (Rewrite, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		domain, answer, ip_version, state, agh_present, first_synced, last_synced, last_error, updated_at
		FROM rewrites WHERE domain = ? AND answer = ? AND ip_version = ?`, domain, answer, ipVersion)
	rw, err := scanRewrite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Rewrite{}, ErrNotFound
	}
	return rw, err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseUTC(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
