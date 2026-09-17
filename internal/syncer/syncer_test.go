package syncer_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cf-opt-adguard/internal/adguard"
	"cf-opt-adguard/internal/planner"
	"cf-opt-adguard/internal/state"
	"cf-opt-adguard/internal/syncer"
)

// ---- httptest 假 AGH：带内存 rewrite 存储 + 故障注入 ----

type aghStore struct {
	mu      sync.Mutex
	entries []adguard.RewriteEntry

	failAdds        int    // 前 N 次 add 返回 500（重试路径）
	failDomain      string // 该域名的 add 恒 500（单条失败隔离）
	forbiddenWrites bool   // 全部写调用 403（认证中止）
	writes          map[string]int
}

func newFakeAGH(t *testing.T, entries ...adguard.RewriteEntry) (string, *aghStore) {
	t.Helper()
	st := &aghStore{entries: entries, writes: map[string]int{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/control/rewrite/list", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		out := append([]adguard.RewriteEntry{}, st.entries...)
		writeJSON(w, out)
	})

	mux.HandleFunc("/control/rewrite/add", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Domain string `json:"domain"`
			Answer string `json:"answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Domain == "" || body.Answer == "" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		st.writes["add"]++
		switch {
		case st.forbiddenWrites:
			w.WriteHeader(http.StatusForbidden)
			return
		case st.failDomain == body.Domain:
			w.WriteHeader(http.StatusInternalServerError)
			return
		case st.failAdds > 0:
			st.failAdds--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		for _, e := range st.entries {
			if e.Domain == body.Domain && e.Answer == body.Answer {
				http.Error(w, "duplicate", http.StatusConflict)
				return
			}
		}
		st.entries = append(st.entries, adguard.RewriteEntry{Domain: body.Domain, Answer: body.Answer})
		writeJSON(w, st.entries)
	})

	mux.HandleFunc("/control/rewrite/update", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Target struct {
				Domain string `json:"domain"`
				Answer string `json:"answer"`
			} `json:"target"`
			Update struct {
				Domain string `json:"domain"`
				Answer string `json:"answer"`
			} `json:"update"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
			body.Target.Domain == "" || body.Target.Answer == "" ||
			body.Update.Domain == "" || body.Update.Answer == "" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		st.writes["update"]++
		if st.forbiddenWrites {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		for i, e := range st.entries {
			if e.Domain == body.Target.Domain && e.Answer == body.Target.Answer {
				st.entries[i].Answer = body.Update.Answer
				writeJSON(w, st.entries)
				return
			}
		}
		http.Error(w, "target not found", http.StatusNotFound)
	})

	mux.HandleFunc("/control/rewrite/delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Domain string `json:"domain"`
			Answer string `json:"answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Domain == "" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		st.writes["delete"]++
		if st.forbiddenWrites {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		for i, e := range st.entries {
			if e.Domain == body.Domain && e.Answer == body.Answer {
				st.entries = append(st.entries[:i:i], st.entries[i+1:]...)
				writeJSON(w, st.entries)
				return
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, st
}

func (s *aghStore) snapshot() []adguard.RewriteEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]adguard.RewriteEntry{}, s.entries...)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- 测试脚手架 ----

const (
	newIP  = "3.3.3.3" // 优选 IP
	newIP6 = "2606:4700::1"
)

func openDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("打开状态库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustUpsert(t *testing.T, db *state.DB, r state.Rewrite) {
	t.Helper()
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = time.Now()
	}
	if err := db.UpsertRewrite(context.Background(), r); err != nil {
		t.Fatalf("UpsertRewrite %s: %v", r.Domain, err)
	}
}

func newSyncer(url string) *syncer.Syncer {
	return syncer.New(adguard.New(url, "u", "p", "", 5*time.Second),
		time.Millisecond, 2, slog.New(slog.DiscardHandler))
}

func getRewrite(t *testing.T, db *state.DB, domain, answer string, ver int) state.Rewrite {
	t.Helper()
	rw, err := db.GetRewrite(context.Background(), domain, answer, ver)
	if err != nil {
		t.Fatalf("GetRewrite %s/%s: %v", domain, answer, err)
	}
	return rw
}

// ---- 测试 ----

// TestSyncPlanEndToEnd 计划全类型执行：add / update / remove 各就位，
// 状态库 active/removed 对齐，用户手工条目不触碰。
func TestSyncPlanEndToEnd(t *testing.T) {
	url, st := newFakeAGH(t,
		adguard.RewriteEntry{Domain: "a.test", Answer: "1.1.1.1"},
		adguard.RewriteEntry{Domain: "b.test", Answer: "9.9.9.9"},
		adguard.RewriteEntry{Domain: "manual.test", Answer: "192.168.1.1"},
	)
	db := openDB(t)
	mustUpsert(t, db, state.Rewrite{Domain: "a.test", Answer: "1.1.1.1", IPVersion: 4, State: "active", AGHPresent: true})
	mustUpsert(t, db, state.Rewrite{Domain: "b.test", Answer: newIP, IPVersion: 4, State: "pending"})
	mustUpsert(t, db, state.Rewrite{Domain: "c.test", Answer: newIP, IPVersion: 4, State: "pending"})

	plan := planner.Plan{
		Add:    []planner.Entry{{Action: planner.ActionAdd, Domain: "c.test", Answer: newIP, IPVersion: 4}},
		Update: []planner.Entry{{Action: planner.ActionUpdate, Domain: "b.test", Answer: newIP, IPVersion: 4}},
		Remove: []planner.Entry{{Action: planner.ActionRemove, Domain: "a.test", Answer: "1.1.1.1", IPVersion: 4}},
	}

	res, err := newSyncer(url).Sync(context.Background(), plan, db)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.OK != 3 || res.Fail != 0 {
		t.Fatalf("结果 = %+v, 期望 OK=3 Fail=0", res)
	}

	got := st.snapshot()
	want := []adguard.RewriteEntry{
		{Domain: "b.test", Answer: newIP},
		{Domain: "manual.test", Answer: "192.168.1.1"},
		{Domain: "c.test", Answer: newIP},
	}
	if len(got) != len(want) {
		t.Fatalf("AGH 条目 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AGH 条目[%d] = %v, 期望 %v", i, got[i], want[i])
		}
	}
	if st.writes["add"] != 1 || st.writes["update"] != 1 || st.writes["delete"] != 1 {
		t.Fatalf("写调用计数 = %v, 期望 add=1 update=1 delete=1", st.writes)
	}

	for _, k := range []struct{ d, a string }{{"c.test", newIP}, {"b.test", newIP}} {
		rw := getRewrite(t, db, k.d, k.a, 4)
		if rw.State != "active" || !rw.AGHPresent || rw.LastSynced.IsZero() || rw.LastError != "" {
			t.Fatalf("%s 状态行应 active/已同步: %+v", k.d, rw)
		}
	}
	if rw := getRewrite(t, db, "a.test", "1.1.1.1", 4); rw.State != "removed" || rw.AGHPresent {
		t.Fatalf("a.test 旧行应 removed 归档: %+v", rw)
	}
}

// TestSyncIdempotentRerun 现状已满足时整批幂等跳过：零写调用，全部计成功。
func TestSyncIdempotentRerun(t *testing.T) {
	url, st := newFakeAGH(t,
		adguard.RewriteEntry{Domain: "a.test", Answer: newIP},
	)
	db := openDB(t)
	mustUpsert(t, db, state.Rewrite{Domain: "a.test", Answer: newIP, IPVersion: 4, State: "active", AGHPresent: true})

	plan := planner.Plan{
		Add:    []planner.Entry{{Action: planner.ActionAdd, Domain: "a.test", Answer: newIP, IPVersion: 4}},
		Remove: []planner.Entry{{Action: planner.ActionRemove, Domain: "a.test", Answer: "1.1.1.1", IPVersion: 4}},
	}
	res, err := newSyncer(url).Sync(context.Background(), plan, db)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.OK != 2 || res.Fail != 0 {
		t.Fatalf("结果 = %+v, 期望 OK=2 Fail=0", res)
	}
	if len(st.writes) != 0 || len(st.snapshot()) != 1 {
		t.Fatalf("应零写调用: writes=%v entries=%v", st.writes, st.snapshot())
	}
}

// TestSyncRetryThenSuccess 首次 500 重试后成功：不计失败。
func TestSyncRetryThenSuccess(t *testing.T) {
	url, st := newFakeAGH(t)
	st.mu.Lock()
	st.failAdds = 1
	st.mu.Unlock()
	db := openDB(t)
	mustUpsert(t, db, state.Rewrite{Domain: "a.test", Answer: newIP, IPVersion: 4, State: "pending"})

	plan := planner.Plan{Add: []planner.Entry{{Action: planner.ActionAdd, Domain: "a.test", Answer: newIP, IPVersion: 4}}}
	res, err := newSyncer(url).Sync(context.Background(), plan, db)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.OK != 1 || res.Fail != 0 {
		t.Fatalf("结果 = %+v, 期望重试后成功", res)
	}
	if st.writes["add"] != 2 {
		t.Fatalf("add 调用 = %d, 期望 2（1 失败 + 1 重试）", st.writes["add"])
	}
}

// TestSyncSingleFailureContinues 单条恒失败不中断整批：其余条目照常执行，
// 失败条目 last_error 落库且状态保留 pending。
func TestSyncSingleFailureContinues(t *testing.T) {
	url, st := newFakeAGH(t)
	st.mu.Lock()
	st.failDomain = "bad.test"
	st.mu.Unlock()
	db := openDB(t)
	mustUpsert(t, db, state.Rewrite{Domain: "bad.test", Answer: newIP, IPVersion: 4, State: "pending"})
	mustUpsert(t, db, state.Rewrite{Domain: "ok.test", Answer: newIP, IPVersion: 4, State: "pending"})

	plan := planner.Plan{Add: []planner.Entry{
		{Action: planner.ActionAdd, Domain: "bad.test", Answer: newIP, IPVersion: 4},
		{Action: planner.ActionAdd, Domain: "ok.test", Answer: newIP, IPVersion: 4},
	}}
	res, err := newSyncer(url).Sync(context.Background(), plan, db)
	if err != nil {
		t.Fatalf("单条失败不应中止整批: %v", err)
	}
	if res.OK != 1 || res.Fail != 1 {
		t.Fatalf("结果 = %+v, 期望 OK=1 Fail=1", res)
	}
	if got := st.snapshot(); len(got) != 1 || got[0].Domain != "ok.test" {
		t.Fatalf("仅 ok.test 应写入: %v", got)
	}
	rw := getRewrite(t, db, "bad.test", newIP, 4)
	if rw.State != "pending" || !strings.Contains(rw.LastError, "add") {
		t.Fatalf("bad.test 应保留 pending 且记录 last_error: %+v", rw)
	}
}

// TestSyncAuthAbort 403 认证失败立即中止整批（后续条目不再尝试），返回错误。
func TestSyncAuthAbort(t *testing.T) {
	url, st := newFakeAGH(t)
	st.mu.Lock()
	st.forbiddenWrites = true
	st.mu.Unlock()
	db := openDB(t)
	mustUpsert(t, db, state.Rewrite{Domain: "a.test", Answer: newIP, IPVersion: 4, State: "pending"})
	mustUpsert(t, db, state.Rewrite{Domain: "b.test", Answer: newIP, IPVersion: 4, State: "pending"})

	plan := planner.Plan{Add: []planner.Entry{
		{Action: planner.ActionAdd, Domain: "a.test", Answer: newIP, IPVersion: 4},
		{Action: planner.ActionAdd, Domain: "b.test", Answer: newIP, IPVersion: 4},
	}}
	res, err := newSyncer(url).Sync(context.Background(), plan, db)
	if err == nil {
		t.Fatal("认证失败应返回错误")
	}
	if res.OK != 0 || res.Fail != 1 {
		t.Fatalf("结果 = %+v, 期望首条失败即中止（OK=0 Fail=1）", res)
	}
	if st.writes["add"] != 1 {
		t.Fatalf("add 调用 = %d, 期望中止后不再尝试（1）", st.writes["add"])
	}
}

// TestSyncFamilyCoexist update 只替换同族答案：异族（v6）互补共存不触碰。
func TestSyncFamilyCoexist(t *testing.T) {
	url, st := newFakeAGH(t,
		adguard.RewriteEntry{Domain: "a.test", Answer: "9.9.9.9"},
		adguard.RewriteEntry{Domain: "a.test", Answer: "fd00::1"},
	)
	db := openDB(t)
	mustUpsert(t, db, state.Rewrite{Domain: "a.test", Answer: newIP, IPVersion: 4, State: "pending"})

	plan := planner.Plan{Update: []planner.Entry{
		{Action: planner.ActionUpdate, Domain: "a.test", Answer: newIP, IPVersion: 4},
	}}
	res, err := newSyncer(url).Sync(context.Background(), plan, db)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.OK != 1 || res.Fail != 0 {
		t.Fatalf("结果 = %+v, 期望 OK=1", res)
	}
	got := st.snapshot()
	if len(got) != 2 || got[0].Answer != newIP || got[1].Answer != "fd00::1" {
		t.Fatalf("v4 应被替换、v6 应保留: %v", got)
	}
	if st.writes["update"] != 1 || st.writes["delete"] != 0 || st.writes["add"] != 0 {
		t.Fatalf("写调用 = %v, 期望仅 update", st.writes)
	}
}

// TestSyncUpdateCleansDuplicates 同族多旧答案：首条 update、其余 delete 清理。
func TestSyncUpdateCleansDuplicates(t *testing.T) {
	url, st := newFakeAGH(t,
		adguard.RewriteEntry{Domain: "a.test", Answer: "9.9.9.9"},
		adguard.RewriteEntry{Domain: "a.test", Answer: "8.8.8.8"},
	)
	db := openDB(t)
	mustUpsert(t, db, state.Rewrite{Domain: "a.test", Answer: newIP, IPVersion: 4, State: "pending"})
	mustUpsert(t, db, state.Rewrite{Domain: "a.test", Answer: "8.8.8.8", IPVersion: 4, State: "active", AGHPresent: true})

	plan := planner.Plan{Update: []planner.Entry{
		{Action: planner.ActionUpdate, Domain: "a.test", Answer: newIP, IPVersion: 4},
	}}
	res, err := newSyncer(url).Sync(context.Background(), plan, db)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.OK != 1 || res.Fail != 0 {
		t.Fatalf("结果 = %+v, 期望 OK=1", res)
	}
	got := st.snapshot()
	if len(got) != 1 || got[0].Answer != newIP {
		t.Fatalf("同族旧答案应清理: %v", got)
	}
	if st.writes["update"] != 1 || st.writes["delete"] != 1 {
		t.Fatalf("写调用 = %v, 期望 update=1 delete=1", st.writes)
	}
	if rw := getRewrite(t, db, "a.test", "8.8.8.8", 4); rw.State != "active" {
		// 8.8.8.8 由 update 条目内联清理，其状态行由计划中的同键 remove 条目处理；
		// 本计划未含 remove，保持 active 属预期（下一轮由 remove 计划归档）。
		t.Logf("8.8.8.8 状态行 = %s（未被本计划触碰）", rw.State)
	}
}
