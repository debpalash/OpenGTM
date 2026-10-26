//go:build integration

package audiencesched_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencesched"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	wsVec   = "ws-p10t5-vec"
	wsOther = "ws-p10t5-other"
	idPfx   = "p10t5-"
)

var frozenNow = time.Date(2026, 8, 29, 10, 40, 0, 0, time.UTC)

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{wsVec, wsOther} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM audiences`)
				return err
			})
		}
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE fire_key LIKE 'audience\_refresh:p10t5-%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM audience_schedules WHERE audience_id LIKE 'p10t5-%'`)
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

type audRow struct {
	ID, WS          string
	Enabled         bool
	Interval        int
	Next            *time.Time
	Health, LastErr string
	Failures        int
}

func mkAudience(t *testing.T, ctx context.Context, pool *pgxpool.Pool, a audRow) {
	t.Helper()
	if a.Health == "" {
		a.Health = "unverified"
	}
	var lastErr *string
	if a.LastErr != "" {
		lastErr = &a.LastErr
	}
	err := tenancy.InWorkspace(ctx, pool, a.WS, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audiences (id, workspace_id, name, filters, refresh_enabled, refresh_interval_minutes,
			next_refresh_at, refresh_health, last_refresh_error, consecutive_refresh_failures)
			VALUES ($1,$2,$1,'{}'::json,$3,$4,$5,$6,$7,$8)`,
			a.ID, a.WS, a.Enabled, a.Interval, a.Next, a.Health, lastErr, a.Failures)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readAudience(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id string) (a audRow, ok bool) {
	t.Helper()
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		var le *string
		err := tx.QueryRow(ctx, `SELECT refresh_enabled, refresh_interval_minutes, next_refresh_at, refresh_health,
			last_refresh_error, consecutive_refresh_failures FROM audiences WHERE id = $1`, id).
			Scan(&a.Enabled, &a.Interval, &a.Next, &a.Health, &le, &a.Failures)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		if le != nil {
			a.LastErr = *le
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, ok
}

type jobRow struct {
	Type, Payload, FireKey, Status, Executor string
	Priority, MaxRetries                     int
	NextRunAt                                *time.Time
	WorkspaceID                              *string
}

func insertJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, j jobRow) int64 {
	t.Helper()
	if j.Type == "" {
		j.Type = "audience_refresh"
	}
	if j.Payload == "" {
		j.Payload = "{}"
	}
	if j.Executor == "" {
		j.Executor = "python"
	}
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO jobs (executor, type, payload, status, priority, created_at, next_run_at, max_retries, retry_count, fire_key)
		VALUES ($1,$2,$3::json,$4,1,(now() AT TIME ZONE 'UTC'),$5,3,0,$6) RETURNING id`,
		j.Executor, j.Type, j.Payload, j.Status, j.NextRunAt, j.FireKey).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func jobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []jobRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT type, payload::text, fire_key, status, executor, priority, max_retries, next_run_at, workspace_id
		FROM jobs WHERE fire_key LIKE 'audience\_refresh:p10t5-%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []jobRow
	for rows.Next() {
		var j jobRow
		if err := rows.Scan(&j.Type, &j.Payload, &j.FireKey, &j.Status, &j.Executor, &j.Priority, &j.MaxRetries, &j.NextRunAt, &j.WorkspaceID); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

type mirrorRow struct {
	AudienceID, WS string
	Next           *time.Time
	Enabled        bool
}

func mirrors(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []mirrorRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT audience_id, workspace_id, next_refresh_at, enabled FROM audience_schedules
		WHERE audience_id LIKE 'p10t5-%' ORDER BY audience_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []mirrorRow
	for rows.Next() {
		var m mirrorRow
		if err := rows.Scan(&m.AudienceID, &m.WS, &m.Next, &m.Enabled); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func setMirror(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, ws string, next *time.Time, enabled bool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO audience_schedules (audience_id, workspace_id, next_refresh_at, enabled) VALUES ($1,$2,$3,$4)`,
		id, ws, next, enabled); err != nil {
		t.Fatal(err)
	}
}

func runSchedule(t *testing.T, ctx context.Context, pool *pgxpool.Pool, in audiencesched.ScheduleInput, now time.Time, executor string) (*time.Time, error) {
	t.Helper()
	var got *time.Time
	err := tenancy.InWorkspace(ctx, pool, in.WorkspaceID, func(tx pgx.Tx) error {
		var err error
		got, err = audiencesched.ScheduleNext(ctx, tx, in, now, executor)
		return err
	})
	return got, err
}

// ---- fixture helpers ----

func loadVec(t *testing.T, name string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../audiencerefresh/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	// Namespace every id so the vectors never touch rows of other suites.
	s := strings.NewReplacer("aud-", idPfx+"aud-", "ws-vec", wsVec, "ws-other", wsOther).Replace(string(raw))
	var doc struct {
		Cases []map[string]any `json:"cases"`
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

func parseTS(t *testing.T, s string) time.Time {
	t.Helper()
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		if v, err := time.Parse(layout, s); err == nil {
			return v
		}
	}
	t.Fatalf("bad timestamp %q", s)
	return time.Time{}
}

func tsPtr(t *testing.T, v any) *time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	x := parseTS(t, s).UTC()
	return &x
}

func eqTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func fmtT(p *time.Time) string {
	if p == nil {
		return "<nil>"
	}
	return p.Format(time.RFC3339Nano)
}

// pyInt is int(value or 60 semantics input): None -> 0, float truncated, string
// parsed, bool as 0/1. The caller treats 0 as Python's falsy "use 60".
func pyInt(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func seedJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, list []any) []int64 {
	t.Helper()
	var ids []int64
	for _, raw := range list {
		m := raw.(map[string]any)
		next := tsPtr(t, m["next_run_at"])
		if next == nil { // the Python ORM column default the generator recorded
			d := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			next = &d
		}
		p := "{}"
		if pm, ok := m["payload"]; ok && pm != nil {
			b, _ := json.Marshal(pm)
			p = string(b)
		}
		ids = append(ids, insertJob(t, ctx, pool, jobRow{
			FireKey: m["fire_key"].(string), Status: m["status"].(string), Payload: p, NextRunAt: next,
		}))
	}
	return ids
}

func compareJobs(t *testing.T, got []jobRow, want []any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("jobs: got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		m := w.(map[string]any)
		g := got[i]
		var gp, wp any
		_ = json.Unmarshal([]byte(g.Payload), &gp)
		wp = m["payload"]
		wnext := tsPtr(t, m["next_run_at"])
		if g.FireKey != m["fire_key"] || g.Status != m["status"] || g.Executor != m["executor"] || g.Type != m["type"] ||
			float64(g.Priority) != m["priority"] || float64(g.MaxRetries) != m["max_retries"] ||
			!reflect.DeepEqual(gp, wp) || !eqTime(g.NextRunAt, wnext) || g.WorkspaceID != nil {
			t.Fatalf("job %d: got %+v (next %s), want %+v", i, g, fmtT(g.NextRunAt), m)
		}
	}
}

func compareMirrors(t *testing.T, got []mirrorRow, want []any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("mirror: got %+v want %+v", got, want)
	}
	for i, w := range want {
		m := w.(map[string]any)
		g := got[i]
		if g.AudienceID != m["audience_id"] || g.WS != m["workspace_id"] || g.Enabled != m["enabled"].(bool) || !eqTime(g.Next, tsPtr(t, m["next_refresh_at"])) {
			t.Fatalf("mirror %d: got %+v (%s) want %+v", i, g, fmtT(g.Next), m)
		}
	}
}

// ---- tests ----

func TestScheduleNextMatchesVectors(t *testing.T) {
	pool, ctx := setup(t)
	ran, skipped := 0, 0
	for _, c := range loadVec(t, "schedule_next.json") {
		name := c["name"].(string)
		nowS := c["now"].(string)
		now := parseTS(t, nowS)
		// Python's key carries now's own offset (or none for a naive clock); the real
		// callers always pass an aware UTC clock, which is the only shape Go models.
		if !strings.HasSuffix(nowS, "+00:00") {
			skipped++
			continue
		}
		t.Run(name, func(t *testing.T) {
			_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE fire_key LIKE 'audience\_refresh:p10t5-%'`)
			_, _ = pool.Exec(ctx, `DELETE FROM audience_schedules WHERE audience_id LIKE 'p10t5-%'`)
			_ = tenancy.InWorkspace(ctx, pool, wsVec, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `DELETE FROM audiences`); return err })
			a := c["audience"].(map[string]any)
			id, ws := a["id"].(string), a["workspace_id"].(string)
			enabled := a["refresh_enabled"].(bool)
			mkAudience(t, ctx, pool, audRow{ID: id, WS: ws, Enabled: enabled, Interval: 60})
			seedJobs(t, ctx, pool, c["jobs_before"].([]any))
			if c["mirror_before_exists"].(bool) {
				old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				setMirror(t, ctx, pool, id, ws, &old, !enabled)
			}
			got, err := runSchedule(t, ctx, pool, audiencesched.ScheduleInput{
				ID: id, WorkspaceID: ws, RefreshEnabled: enabled, IntervalMinutes: pyInt(a["refresh_interval_minutes"]),
			}, now, c["executor"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if want := tsPtr(t, c["returned"]); !eqTime(got, want) {
				t.Fatalf("returned %s want %s", fmtT(got), fmtT(want))
			}
			ar, _ := readAudience(t, ctx, pool, ws, id)
			if want := tsPtr(t, c["audience_next_refresh_at"]); !eqTime(ar.Next, want) {
				t.Fatalf("audience next_refresh_at %s want %s", fmtT(ar.Next), fmtT(want))
			}
			compareJobs(t, jobs(t, ctx, pool), c["jobs_after"].([]any))
			compareMirrors(t, mirrors(t, ctx, pool), c["schedule_after"].([]any))
		})
		ran++
	}
	if ran < 25 {
		t.Fatalf("only %d vector cases ran (%d skipped)", ran, skipped)
	}
	t.Logf("ran %d vector cases, skipped %d (non-UTC or naive clocks)", ran, skipped)
}

func TestScheduleNextCancelsPendingOnly(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audRow{ID: idPfx + "a", WS: wsVec, Enabled: true, Interval: 60})
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ws := func(w string) string { return fmt.Sprintf(`{"workspace_id":%q}`, w) }
	k := func(id string) string { return "audience_refresh:" + id + ":2026-02-01T00:00:00+00:00" }
	seed := []jobRow{
		{FireKey: k(idPfx + "a"), Status: "pending", Payload: ws(wsVec)},                // 0: cancelled
		{FireKey: k(idPfx + "a"), Status: "processing", Payload: ws(wsVec)},             // 1: active, untouched
		{FireKey: k(idPfx + "a"), Status: "completed", Payload: ws(wsVec)},              // 2
		{FireKey: k(idPfx + "a:b"), Status: "pending", Payload: ws(wsVec)},              // 3: 'a' must not match 'a:b'
		{FireKey: k(idPfx+"a") + "x", Status: "pending", Payload: ws(wsVec)},            // 4: occurrence-shaped, cancelled
		{FireKey: k(idPfx + "a"), Status: "pending", Payload: ws(wsOther)},              // 5: another tenant's payload
		{FireKey: "audience_refresh:" + idPfx + "a:bootstrap", Status: "pending"},       // 6: bootstrap marker, cancelled
		{FireKey: k(idPfx + "aZ"), Status: "pending", Payload: ws(wsVec)},               // 7: neighbour id
		{FireKey: k(idPfx + "a"), Status: "pending", Payload: ws(wsVec), Type: "other"}, // 8: other job type
	}
	for i := range seed {
		seed[i].NextRunAt = &old
		insertJob(t, ctx, pool, seed[i])
	}
	if _, err := runSchedule(t, ctx, pool, audiencesched.ScheduleInput{ID: idPfx + "a", WorkspaceID: wsVec, RefreshEnabled: false}, frozenNow, "python"); err != nil {
		t.Fatal(err)
	}
	got := jobs(t, ctx, pool)
	// Row 4 ("...+00:00x") still has the occurrence shape of 'a', as under Python's LIKE.
	want := []string{"cancelled", "processing", "completed", "pending", "cancelled", "pending", "cancelled", "pending", "pending"}
	if len(got) != len(want) {
		t.Fatalf("got %d jobs want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Status != want[i] {
			t.Errorf("job %d (%s, %s): status %s want %s", i, got[i].FireKey, got[i].Payload, got[i].Status, want[i])
		}
	}
}

func TestScheduleNextLikeWildcardsInIDAreLiteral(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audRow{ID: idPfx + "u_1", WS: wsVec, Enabled: true, Interval: 60})
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insertJob(t, ctx, pool, jobRow{FireKey: "audience_refresh:" + idPfx + "uX1:2026-02-01T00:00:00+00:00", Status: "pending", NextRunAt: &old})
	insertJob(t, ctx, pool, jobRow{FireKey: "audience_refresh:" + idPfx + "u_1:2026-02-01T00:00:00+00:00", Status: "pending", NextRunAt: &old})
	if _, err := runSchedule(t, ctx, pool, audiencesched.ScheduleInput{ID: idPfx + "u_1", WorkspaceID: wsVec, RefreshEnabled: false}, frozenNow, "python"); err != nil {
		t.Fatal(err)
	}
	got := jobs(t, ctx, pool)
	if got[0].Status != "pending" || got[1].Status != "cancelled" {
		t.Fatalf("wildcard overmatch: %+v", got)
	}
}

func TestScheduleNextDisabledClears(t *testing.T) {
	pool, ctx := setup(t)
	next := frozenNow.Add(time.Hour)
	mkAudience(t, ctx, pool, audRow{ID: idPfx + "d", WS: wsVec, Enabled: false, Interval: 60, Next: &next})
	setMirror(t, ctx, pool, idPfx+"d", wsVec, &next, true)
	got, err := runSchedule(t, ctx, pool, audiencesched.ScheduleInput{ID: idPfx + "d", WorkspaceID: wsVec, RefreshEnabled: false, IntervalMinutes: 60}, frozenNow, "go")
	if err != nil || got != nil {
		t.Fatalf("got %v err %v, want nil", got, err)
	}
	if a, _ := readAudience(t, ctx, pool, wsVec, idPfx+"d"); a.Next != nil {
		t.Fatalf("next_refresh_at not cleared: %v", a.Next)
	}
	m := mirrors(t, ctx, pool)
	if len(m) != 1 || m[0].Next != nil || m[0].Enabled {
		t.Fatalf("mirror: %+v", m)
	}
	if len(jobs(t, ctx, pool)) != 0 {
		t.Fatal("disabled schedule enqueued a job")
	}
}

func TestIntervalClamped(t *testing.T) {
	pool, ctx := setup(t)
	cases := []struct{ in, wantMin int }{
		{-5, 15}, {1, 15}, {14, 15}, {15, 15}, {16, 16}, {60, 60}, {0, 60}, {10080, 10080}, {10081, 10080}, {99999, 10080},
	}
	for _, c := range cases {
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE fire_key LIKE 'audience\_refresh:p10t5-%'`)
		id := idPfx + "iv"
		_ = tenancy.InWorkspace(ctx, pool, wsVec, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `DELETE FROM audiences`); return err })
		mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 60})
		got, err := runSchedule(t, ctx, pool, audiencesched.ScheduleInput{ID: id, WorkspaceID: wsVec, RefreshEnabled: true, IntervalMinutes: c.in}, frozenNow, "python")
		if err != nil {
			t.Fatal(err)
		}
		want := frozenNow.Add(time.Duration(c.wantMin) * time.Minute)
		if got == nil || !got.Equal(want) {
			t.Errorf("interval %d: got %s want %s", c.in, fmtT(got), want)
		}
	}
}

func TestScheduleSingleFlightConcurrent(t *testing.T) {
	pool, ctx := setup(t)
	id := idPfx + "sf"
	mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 60})
	in := audiencesched.ScheduleInput{ID: id, WorkspaceID: wsVec, RefreshEnabled: true, IntervalMinutes: 60}
	var wg sync.WaitGroup
	errs := make([]error, 16)
	res := make([]*time.Time, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], errs[i] = runSchedule(t, ctx, pool, in, frozenNow, "python")
		}(i)
	}
	wg.Wait()
	want := frozenNow.Add(time.Hour)
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if res[i] == nil || !res[i].Equal(want) {
			t.Fatalf("goroutine %d returned %s", i, fmtT(res[i]))
		}
	}
	pending := 0
	for _, j := range jobs(t, ctx, pool) {
		if j.Status == "pending" {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("pending audience_refresh jobs = %d, want exactly 1", pending)
	}
	if m := mirrors(t, ctx, pool); len(m) != 1 {
		t.Fatalf("mirror rows %d", len(m))
	}
}

func TestScheduleConcurrentDistinctClocksLeaveOnePending(t *testing.T) {
	pool, ctx := setup(t)
	id := idPfx + "sf2"
	mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 60})
	in := audiencesched.ScheduleInput{ID: id, WorkspaceID: wsVec, RefreshEnabled: true, IntervalMinutes: 60}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := runSchedule(t, ctx, pool, in, frozenNow.Add(time.Duration(i)*time.Second), "python"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	pending := 0
	for _, j := range jobs(t, ctx, pool) {
		if j.Status == "pending" {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1 (each call cancels its predecessor)", pending)
	}
}

func TestScheduleRollsBackWithTx(t *testing.T) {
	pool, ctx := setup(t)
	id := idPfx + "rb"
	mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 60})
	boom := fmt.Errorf("boom")
	err := tenancy.InWorkspace(ctx, pool, wsVec, func(tx pgx.Tx) error {
		if _, err := audiencesched.ScheduleNext(ctx, tx, audiencesched.ScheduleInput{ID: id, WorkspaceID: wsVec, RefreshEnabled: true, IntervalMinutes: 60}, frozenNow, "python"); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("err = %v", err)
	}
	if len(jobs(t, ctx, pool)) != 0 || len(mirrors(t, ctx, pool)) != 0 {
		t.Fatal("job or mirror survived rollback")
	}
	if a, _ := readAudience(t, ctx, pool, wsVec, id); a.Next != nil {
		t.Fatalf("audience next_refresh_at survived rollback: %v", a.Next)
	}
}

func TestScheduleNextRejectsForeignWorkspaceAndBadExecutor(t *testing.T) {
	pool, ctx := setup(t)
	id := idPfx + "ws"
	mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 60})
	if _, err := runSchedule(t, ctx, pool, audiencesched.ScheduleInput{ID: id, WorkspaceID: wsVec, RefreshEnabled: true, IntervalMinutes: 60}, frozenNow, "rust"); err == nil {
		t.Fatal("bad executor accepted")
	}
	// Input names workspace B while the transaction is tenant A.
	err := tenancy.InWorkspace(ctx, pool, wsVec, func(tx pgx.Tx) error {
		_, err := audiencesched.ScheduleNext(ctx, tx, audiencesched.ScheduleInput{ID: id, WorkspaceID: wsOther, RefreshEnabled: true, IntervalMinutes: 60}, frozenNow, "python")
		return err
	})
	if err == nil {
		t.Fatal("workspace mismatch accepted")
	}
	if len(jobs(t, ctx, pool)) != 0 || len(mirrors(t, ctx, pool)) != 0 {
		t.Fatal("a rejected call wrote rows")
	}
}

func TestRemoveScheduleDeletesMirror(t *testing.T) {
	pool, ctx := setup(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setMirror(t, ctx, pool, idPfx+"r1", wsVec, &old, true)
	setMirror(t, ctx, pool, idPfx+"r2", wsVec, &old, true)
	setMirror(t, ctx, pool, idPfx+"r3", wsOther, &old, true) // another tenant's row
	k := func(id string) string { return "audience_refresh:" + id + ":2026-02-01T00:00:00+00:00" }
	insertJob(t, ctx, pool, jobRow{FireKey: k(idPfx + "r1"), Status: "pending", NextRunAt: &old})
	insertJob(t, ctx, pool, jobRow{FireKey: k(idPfx + "r1"), Status: "processing", NextRunAt: &old})
	insertJob(t, ctx, pool, jobRow{FireKey: k(idPfx + "r2"), Status: "pending", NextRunAt: &old})
	err := tenancy.InWorkspace(ctx, pool, wsVec, func(tx pgx.Tx) error {
		return audiencesched.RemoveSchedule(ctx, tx, idPfx+"r1")
	})
	if err != nil {
		t.Fatal(err)
	}
	m := mirrors(t, ctx, pool)
	if len(m) != 2 || m[0].AudienceID != idPfx+"r2" || m[1].AudienceID != idPfx+"r3" {
		t.Fatalf("mirror rows: %+v", m)
	}
	j := jobs(t, ctx, pool)
	if j[0].Status != "cancelled" || j[1].Status != "processing" || j[2].Status != "pending" {
		t.Fatalf("jobs: %+v", j)
	}
	// A foreign tenant cannot delete another tenant's mirror row.
	_ = tenancy.InWorkspace(ctx, pool, wsVec, func(tx pgx.Tx) error {
		return audiencesched.RemoveSchedule(ctx, tx, idPfx+"r3")
	})
	if len(mirrors(t, ctx, pool)) != 2 {
		t.Fatal("RemoveSchedule deleted another workspace's mirror row")
	}
}

func TestDueSchedules(t *testing.T) {
	pool, ctx := setup(t)
	past := frozenNow.Add(-time.Minute)
	exact := frozenNow
	future := frozenNow.Add(time.Minute)
	setMirror(t, ctx, pool, idPfx+"due-null", wsVec, nil, true)
	setMirror(t, ctx, pool, idPfx+"due-past", wsOther, &past, true)
	setMirror(t, ctx, pool, idPfx+"due-exact", wsVec, &exact, true)
	setMirror(t, ctx, pool, idPfx+"due-future", wsVec, &future, true)
	setMirror(t, ctx, pool, idPfx+"due-off", wsVec, &past, false)
	rows, err := audiencesched.DueSchedules(ctx, pool, frozenNow)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		if strings.HasPrefix(r.AudienceID, idPfx) {
			ids = append(ids, r.AudienceID)
		}
	}
	want := []string{idPfx + "due-exact", idPfx + "due-null", idPfx + "due-past"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("due = %v want %v", ids, want)
	}
	for _, r := range rows {
		if r.AudienceID == idPfx+"due-past" && (r.WorkspaceID != wsOther || r.NextRefreshAt == nil || !r.NextRefreshAt.Equal(past)) {
			t.Fatalf("row %+v", r)
		}
	}
}

// ---- reconcile ----

func runReconcile(ctx context.Context, pool *pgxpool.Pool, jobID int64, payload map[string]any, errText string, willRetry bool) error {
	return audiencesched.ReconcileFailure(ctx, pool, func(string) string { return "python" }, jobID, payload, errText, willRetry, frozenNow)
}

func TestReconcileFailureMatchesVectors(t *testing.T) {
	pool, ctx := setup(t)
	ran := 0
	for _, c := range loadVec(t, "failure_reconcile.json") {
		name := c["name"].(string)
		t.Run(name, func(t *testing.T) {
			_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE fire_key LIKE 'audience\_refresh:p10t5-%'`)
			_, _ = pool.Exec(ctx, `DELETE FROM audience_schedules WHERE audience_id LIKE 'p10t5-%'`)
			_ = tenancy.InWorkspace(ctx, pool, wsVec, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `DELETE FROM audiences`); return err })
			id := idPfx + "aud-1"
			if ab, ok := c["audience_before"].(map[string]any); ok {
				a := audRow{ID: id, WS: wsVec, Enabled: ab["refresh_enabled"].(bool), Interval: int(ab["refresh_interval_minutes"].(float64)), Next: tsPtr(t, ab["next_refresh_at"])}
				if v, ok := ab["consecutive_refresh_failures"].(float64); ok {
					a.Failures = int(v)
				}
				if v, ok := ab["refresh_health"].(string); ok {
					a.Health = v
				}
				if v, ok := ab["last_refresh_error"].(string); ok {
					a.LastErr = v
				}
				mkAudience(t, ctx, pool, a)
			}
			jobID := int64(2_000_000_011) // absent from the table when job_before is null
			if jb, ok := c["job_before"].(map[string]any); ok {
				ids := seedJobs(t, ctx, pool, []any{jb})
				jobID = ids[0]
			}
			if mb, ok := c["mirror_before"].(map[string]any); ok {
				setMirror(t, ctx, pool, id, wsVec, tsPtr(t, mb["next_refresh_at"]), mb["enabled"].(bool))
			}
			payload, _ := c["payload"].(map[string]any)
			errText, _ := c["error"].(string)
			err := runReconcile(ctx, pool, jobID, payload, errText, c["will_retry"].(bool))
			if raises, _ := c["raises"].(map[string]any); raises != nil {
				if err == nil || !strings.Contains(err.Error(), raises["message"].(string)) {
					t.Fatalf("err = %v, want %v", err, raises["message"])
				}
			} else if err != nil {
				t.Fatal(err)
			}
			after := c["after"].(map[string]any)
			if wa, ok := after["audience"].(map[string]any); ok {
				ga, found := readAudience(t, ctx, pool, wsVec, id)
				if !found {
					t.Fatal("audience vanished")
				}
				if ga.Health != wa["refresh_health"] || float64(ga.Failures) != wa["consecutive_refresh_failures"] ||
					!eqTime(ga.Next, tsPtr(t, wa["next_refresh_at"])) || ga.Enabled != wa["refresh_enabled"] {
					t.Fatalf("audience: got %+v (%s) want %+v", ga, fmtT(ga.Next), wa)
				}
				wantErr, _ := wa["last_refresh_error"].(string)
				if ga.LastErr != wantErr {
					t.Fatalf("last_refresh_error %q want %q", ga.LastErr, wantErr)
				}
			}
			compareJobs(t, jobs(t, ctx, pool), after["jobs"].([]any))
			compareMirrors(t, mirrors(t, ctx, pool), after["schedule"].([]any))
		})
		ran++
	}
	if ran < 20 {
		t.Fatalf("only %d cases", ran)
	}
}

func TestReconcileFailureRetryMirrorsQueueTime(t *testing.T) {
	pool, ctx := setup(t)
	id := idPfx + "rt"
	prev := frozenNow.Add(-30 * time.Minute)
	mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 60, Next: &prev})
	retry := frozenNow.Add(5 * time.Minute)
	jid := insertJob(t, ctx, pool, jobRow{FireKey: "audience_refresh:" + id + ":2026-08-29T10:00:00+00:00", Status: "pending", NextRunAt: &retry})
	if err := runReconcile(ctx, pool, jid, map[string]any{"workspace_id": wsVec, "audience_id": id}, "boom", true); err != nil {
		t.Fatal(err)
	}
	a, _ := readAudience(t, ctx, pool, wsVec, id)
	if a.Health != "degraded" || a.LastErr != "Queue retry: boom" || a.Failures != 1 || !eqTime(a.Next, &retry) {
		t.Fatalf("audience %+v (%s)", a, fmtT(a.Next))
	}
	m := mirrors(t, ctx, pool)
	if len(m) != 1 || !eqTime(m[0].Next, &retry) || !m[0].Enabled {
		t.Fatalf("mirror %+v", m)
	}
	if j := jobs(t, ctx, pool); len(j) != 1 || j[0].Status != "pending" {
		t.Fatalf("retry must not cancel the queue's job: %+v", j)
	}
}

func TestReconcileRetryJobWithoutNextRunKeepsSchedule(t *testing.T) {
	pool, ctx := setup(t)
	id := idPfx + "rn"
	prev := frozenNow.Add(-30 * time.Minute)
	mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 60, Next: &prev})
	jid := insertJob(t, ctx, pool, jobRow{FireKey: "audience_refresh:" + id + ":2026-08-29T10:00:00+00:00", Status: "pending"})
	if err := runReconcile(ctx, pool, jid, map[string]any{"workspace_id": wsVec, "audience_id": id}, "boom", true); err != nil {
		t.Fatal(err)
	}
	a, _ := readAudience(t, ctx, pool, wsVec, id)
	if a.Failures != 1 || a.Health != "degraded" || !eqTime(a.Next, &prev) || len(mirrors(t, ctx, pool)) != 0 {
		t.Fatalf("audience %+v (%s)", a, fmtT(a.Next))
	}
}

func TestReconcileFailureTerminalSchedulesNext(t *testing.T) {
	pool, ctx := setup(t)
	id := idPfx + "tm"
	prev := frozenNow.Add(-30 * time.Minute)
	mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 30, Next: &prev})
	retry := frozenNow.Add(5 * time.Minute)
	jid := insertJob(t, ctx, pool, jobRow{FireKey: "audience_refresh:" + id + ":2026-08-29T10:00:00+00:00", Status: "pending", NextRunAt: &retry})
	var long = strings.Repeat("é", 1500)
	err := audiencesched.ReconcileFailure(ctx, pool, func(ws string) string {
		if ws != wsVec {
			t.Errorf("executor asked for %q", ws)
		}
		return "go"
	}, jid, map[string]any{"workspace_id": " " + wsVec + " ", "audience_id": id}, long, false, frozenNow)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := readAudience(t, ctx, pool, wsVec, id)
	next := frozenNow.Add(30 * time.Minute)
	if a.Health != "degraded" || a.Failures != 1 || !eqTime(a.Next, &next) {
		t.Fatalf("audience %+v (%s)", a, fmtT(a.Next))
	}
	if r := []rune(a.LastErr); len(r) != 1000 || !strings.HasPrefix(a.LastErr, "Final failure: é") {
		t.Fatalf("last_refresh_error has %d runes: %.30q", len(r), a.LastErr)
	}
	j := jobs(t, ctx, pool)
	if len(j) != 2 || j[1].Status != "pending" || j[1].Executor != "go" || !eqTime(j[1].NextRunAt, &next) ||
		j[1].FireKey != "audience_refresh:"+id+":2026-08-29T11:10:00+00:00" {
		t.Fatalf("jobs %+v", j)
	}
}

func TestReconcileFailureAlreadyReconciledNoDoubleCount(t *testing.T) {
	pool, ctx := setup(t)
	id := idPfx + "ar"
	future := frozenNow.Add(30 * time.Minute)
	mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 60, Next: &future, Health: "degraded", LastErr: "handler wrote this", Failures: 1})
	retry := frozenNow.Add(5 * time.Minute)
	jid := insertJob(t, ctx, pool, jobRow{FireKey: "audience_refresh:" + id + ":2026-08-29T10:00:00+00:00", Status: "pending", NextRunAt: &retry})
	p := map[string]any{"workspace_id": wsVec, "audience_id": id}
	for _, willRetry := range []bool{true, false, true} {
		if err := runReconcile(ctx, pool, jid, p, "again", willRetry); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := readAudience(t, ctx, pool, wsVec, id)
	if a.Failures != 1 || a.LastErr != "handler wrote this" || !eqTime(a.Next, &future) {
		t.Fatalf("double counted: %+v (%s)", a, fmtT(a.Next))
	}
	if len(mirrors(t, ctx, pool)) != 0 || len(jobs(t, ctx, pool)) != 1 {
		t.Fatal("already-reconciled call wrote schedule rows")
	}
}

func TestReconcileFailureOtherWorkspaceUntouched(t *testing.T) {
	pool, ctx := setup(t)
	id := idPfx + "ow"
	prev := frozenNow.Add(-time.Minute)
	mkAudience(t, ctx, pool, audRow{ID: id, WS: wsVec, Enabled: true, Interval: 60, Next: &prev})
	if err := runReconcile(ctx, pool, 1, map[string]any{"workspace_id": wsOther, "audience_id": id}, "x", false); err != nil {
		t.Fatal(err)
	}
	if a, _ := readAudience(t, ctx, pool, wsVec, id); a.Failures != 0 || a.Health != "unverified" {
		t.Fatalf("foreign-workspace payload changed the audience: %+v", a)
	}
}
