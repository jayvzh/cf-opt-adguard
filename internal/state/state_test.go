package state

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"cf-opt-adguard/internal/aggregate"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	s := openTest(t)
	// 二次打开不重复应用迁移、不报错。
	if err := s.migrate(); err != nil {
		t.Fatalf("重复迁移失败: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("迁移记录数 %d want 1", n)
	}
}

func TestDomainUpsertKeepsFirstSeen(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	t0 := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)

	if err := s.UpsertDomains(ctx, []Domain{
		{Domain: "example.com", Zone: "example.com", HitCount: 30, FirstSeen: t0, LastSeen: t0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDomains(ctx, []Domain{
		{Domain: "example.com", Zone: "example.com", HitCount: 55, FirstSeen: t1, LastSeen: t1},
	}); err != nil {
		t.Fatal(err)
	}

	var hit int
	var first, last string
	if err := s.db.QueryRow(
		`SELECT hit_count, first_seen, last_seen FROM domains WHERE domain = 'example.com'`,
	).Scan(&hit, &first, &last); err != nil {
		t.Fatal(err)
	}
	if hit != 55 {
		t.Errorf("hit_count=%d want 55（重算覆盖）", hit)
	}
	if first != "2026-09-01T08:00:00Z" {
		t.Errorf("first_seen=%s want 保留首次值", first)
	}
	if last != "2026-09-02T08:00:00Z" {
		t.Errorf("last_seen=%s want 更新", last)
	}
}

func TestProbeUpsertRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now().UTC()
	err := s.UpsertProbes(ctx, []Probe{{
		Domain:      "example.com",
		CNAMEHit:    true,
		CIDRHit:     true,
		CFRayHit:    true,
		ServerHit:   false,
		Score:       7,
		Confidence:  aggregate.VerdictConfirmed,
		CNAMEChain:  "example.com -> host.examplecdn.cloudflare.net",
		ResolvedIPs: []net.IP{net.ParseIP("104.16.0.1"), net.ParseIP("2606:4700::1")},
		ProbedAt:    now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var score, cname, cidr, cfRay, server int
	var confidence, ipsJSON string
	if err := s.db.QueryRow(`SELECT cname_hit, cidr_hit, cf_ray_hit, server_hit, score,
		confidence, resolved_ips FROM domain_probes WHERE domain = 'example.com'`,
	).Scan(&cname, &cidr, &cfRay, &server, &score, &confidence, &ipsJSON); err != nil {
		t.Fatal(err)
	}
	if score != 7 || confidence != "confirmed" {
		t.Errorf("score=%d confidence=%s", score, confidence)
	}
	if cname != 1 || cidr != 1 || cfRay != 1 || server != 0 {
		t.Errorf("信号列: %d/%d/%d/%d", cname, cidr, cfRay, server)
	}
	want := `["104.16.0.1","2606:4700::1"]`
	if ipsJSON != want {
		t.Errorf("resolved_ips=%s want %s", ipsJSON, want)
	}
}

func TestRewriteLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now().UTC()

	rw := Rewrite{
		Domain: "example.com", Answer: "104.21.88.54", IPVersion: 4,
		State: "pending", FirstSynced: now, LastSynced: now, UpdatedAt: now,
	}
	if err := s.UpsertRewrite(ctx, rw); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRewrite(ctx, rw.Domain, rw.Answer, rw.IPVersion)
	if err != nil || got.State != "pending" || got.AGHPresent {
		t.Fatalf("get: %+v err=%v", got, err)
	}

	// 托管集合包含 pending。
	managed, err := s.ListManaged(ctx)
	if err != nil || !managed["example.com"] {
		t.Fatalf("managed=%v err=%v", managed, err)
	}

	// 状态流转 active + agh_present。
	rw.State = "active"
	rw.AGHPresent = true
	rw.UpdatedAt = now.Add(time.Minute)
	if err := s.UpsertRewrite(ctx, rw); err != nil {
		t.Fatal(err)
	}
	actives, err := s.ListRewritesByState(ctx, "active")
	if err != nil || len(actives) != 1 || !actives[0].AGHPresent {
		t.Fatalf("actives=%+v err=%v", actives, err)
	}

	// removed 后退出托管集合。
	if err := s.SetRewriteState(ctx, rw.Domain, rw.Answer, rw.IPVersion, "removed", ""); err != nil {
		t.Fatal(err)
	}
	managed, _ = s.ListManaged(ctx)
	if managed["example.com"] {
		t.Fatal("removed 后仍在托管集合")
	}

	// 复合主键：同 domain 不同 ip_version 共存。
	rw6 := Rewrite{Domain: "example.com", Answer: "2606:4700::1", IPVersion: 6, State: "pending", UpdatedAt: now}
	if err := s.UpsertRewrite(ctx, rw6); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRewrite(ctx, rw6.Domain, rw6.Answer, rw6.IPVersion); err != nil {
		t.Fatalf("v6 行应存在: %v", err)
	}

	if _, err := s.GetRewrite(ctx, "nope.com", "1.2.3.4", 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestRunAuditAndTrim(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	start := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	// 插入 MaxRuns+5 条，护栏应裁到 MaxRuns。
	var lastID int64
	for i := 0; i < MaxRuns+5; i++ {
		id, err := s.InsertRun(ctx, Run{Mode: "dry", StartedAt: start.Add(time.Duration(i) * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		lastID = id
	}
	removed, err := s.EnforceCaps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 5 {
		t.Fatalf("removed=%d want 5", removed)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
	if n != MaxRuns {
		t.Fatalf("runs=%d want %d", n, MaxRuns)
	}

	logEntries, candidates, confirmed := 1200, 8, 5
	if err := s.FinishRun(ctx, lastID, Run{
		FinishedAt: start.Add(time.Hour), LogEntries: &logEntries,
		Candidates: &candidates, Confirmed: &confirmed,
		NAdd: 2, PreferredIP: "104.21.88.54", Note: "dry-run 完成",
	}); err != nil {
		t.Fatal(err)
	}
	var mode, note, preferred string
	if err := s.db.QueryRow(
		`SELECT mode, note, preferred_ip FROM runs WHERE id = ?`, lastID,
	).Scan(&mode, &note, &preferred); err != nil {
		t.Fatal(err)
	}
	if mode != "dry" || note != "dry-run 完成" || preferred != "104.21.88.54" {
		t.Errorf("mode=%s note=%s ip=%s", mode, note, preferred)
	}
}

func TestDomainsCap(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	base := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)

	ds := make([]Domain, 0, MaxDomains+10)
	for i := 0; i < MaxDomains+10; i++ {
		d := Domain{
			Domain:    domainN(i),
			Zone:      domainN(i),
			HitCount:  1,
			FirstSeen: base,
			LastSeen:  base.Add(time.Duration(i) * time.Second), // 越新越靠后
		}
		ds = append(ds, d)
	}
	if err := s.UpsertDomains(ctx, ds); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnforceCaps(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM domains`).Scan(&n)
	if n != MaxDomains {
		t.Fatalf("domains=%d want %d", n, MaxDomains)
	}
	// 最旧的 10 个（i=0..9）应被淘汰。
	var gone int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM domains WHERE domain = ?`, domainN(0)).Scan(&gone)
	if gone != 0 {
		t.Fatal("最旧域名未被淘汰")
	}
}

func domainN(i int) string {
	return "d" + pad(i, 5) + ".example.com"
}

func pad(i, w int) string {
	b := []byte{}
	for i > 0 || len(b) < w {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
