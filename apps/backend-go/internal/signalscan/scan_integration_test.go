//go:build integration

package signalscan_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/signalscan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

var fixedNow = time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC) // ISO week 2026-W35

type env struct {
	pool, owner *pgxpool.Pool
	prefix      string
	wss         []string
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{pool: testpg.NewAppPool(t, 8), prefix: fmt.Sprintf("t17_%d_", os.Getpid())}
	var err error
	e.owner, err = pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.owner.Close)
	e.clean()
	t.Cleanup(e.clean)
	return e
}

func (e *env) clean() {
	ctx := context.Background()
	pat := e.prefix + "%"
	for _, q := range []string{
		`DELETE FROM workbook_activity WHERE workspace_id LIKE $1`,
		`DELETE FROM workbook_rows WHERE workspace_id LIKE $1`,
		`DELETE FROM workbooks WHERE workspace_id LIKE $1`,
		`DELETE FROM triggers WHERE workspace_id LIKE $1`,
		`DELETE FROM signals WHERE workspace_id LIKE $1`,
		`DELETE FROM leads WHERE workspace_id LIKE $1`,
		`DELETE FROM workspaces WHERE id LIKE $1`,
		`DELETE FROM jobs WHERE type IN ('trigger_eval','refresh_workbook') AND workspace_id LIKE $1`,
		`DELETE FROM jobs WHERE type = 'signal_scan' AND fire_key LIKE 'signal_scan:2026-08-27T%' AND $1::text IS NOT NULL`,
	} {
		_, _ = e.owner.Exec(ctx, q, pat)
	}
}

func (e *env) workspace(t *testing.T, name string) string {
	t.Helper()
	id := e.prefix + name
	if _, err := e.owner.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ($1::text,$1::text,$1::text, extract(epoch from clock_timestamp()), extract(epoch from now()))`, id); err != nil {
		t.Fatal(err)
	}
	e.wss = append(e.wss, id)
	return id
}

func (e *env) lead(t *testing.T, ws, company string, score int, tier string) int64 {
	t.Helper()
	var id int64
	if err := e.owner.QueryRow(context.Background(),
		`INSERT INTO leads (workspace_id, company, score, score_tier, city) VALUES ($1,$2,$3,$4,'') RETURNING id`, ws, company, score, tier).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) score(t *testing.T, ws string, lead int64) (score int, tier string) {
	t.Helper()
	if err := e.owner.QueryRow(context.Background(), `SELECT score, score_tier FROM leads WHERE id=$1 AND workspace_id=$2`, lead, ws).Scan(&score, &tier); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *env) count(t *testing.T, q string, args ...any) (n int) {
	t.Helper()
	if err := e.owner.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return
}

// hiringPy answers the jobspy provider call by company.
type hiringPy struct {
	jobs   map[string]int
	titles map[string][]string
	calls  int
}

func (h *hiringPy) Call(_ context.Context, c pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	h.calls++
	var in struct {
		Company string `json:"company"`
	}
	_ = json.Unmarshal(c.Input, &in)
	n, ok := h.jobs[in.Company]
	if !ok {
		return pyworker.ProviderResult{Status: "error", Output: json.RawMessage(`{"success":false,"fields":{}}`)}, nil
	}
	body := map[string]any{"total_jobs": n}
	if t, ok := h.titles[in.Company]; ok {
		body["top_titles"] = t
	}
	hs, _ := json.Marshal(body)
	inner, _ := json.Marshal(string(hs)) // JobSpy returns the JSON as a string
	return pyworker.ProviderResult{Status: "ok", Output: json.RawMessage(`{"success":true,"fields":{"hiring_signals":` + string(inner) + `}}`)}, nil
}

func (e *env) deps(py signalscan.Caller) signalscan.Deps {
	return signalscan.Deps{
		Pool: e.pool, Py: py, Hook: automationhook.New(true, slog.Default()), Log: slog.Default(),
		Now: func() time.Time { return fixedNow },
	}
}

func (e *env) signalIDs(t *testing.T, ws string) []string {
	t.Helper()
	var out []string
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id FROM signals ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScanIsIdempotentAcrossRuns(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	l1 := e.lead(t, ws, "Acme", 40, "hot")
	l2 := e.lead(t, ws, "Beta", 10, "warm")
	py := &hiringPy{jobs: map[string]int{"Acme": 30, "Beta": 3}, titles: map[string][]string{"Acme": {"SDR", "AE"}}}
	d := e.deps(py)

	c1, err := signalscan.Run(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if c1.SignalsFound < 2 || c1.SignalsInserted < 2 {
		t.Fatalf("first run %+v", c1)
	}
	ids := e.signalIDs(t, ws)
	want1 := signalscan.ScanSignalID(ws, l1, "hiring", "high", fixedNow)
	want2 := signalscan.ScanSignalID(ws, l2, "hiring", "low", fixedNow)
	if len(ids) != 2 || !(ids[0] == want1 && ids[1] == want2 || ids[0] == want2 && ids[1] == want1) {
		t.Fatalf("ids %v want %s %s", ids, want1, want2)
	}
	s1, tier1 := e.score(t, ws, l1)
	if s1 != 50 || tier1 != "warm" { // 40 + weight 10
		t.Fatalf("lead1 boosted to %d/%s", s1, tier1)
	}

	c2, err := signalscan.Run(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if c2.SignalsFound < 1 || c2.SignalsInserted != 0 {
		t.Fatalf("second run inserted again: %+v", c2)
	}
	if got := e.signalIDs(t, ws); len(got) != 2 {
		t.Fatalf("rescan changed the row set: %v", got)
	}
	// Python parity: _apply_signal_boosts re-boosts on every rescan (signals_found, not inserted).
	if s2, _ := e.score(t, ws, l1); s2 != 60 {
		t.Fatalf("rescan boost: %d, want 60 (Python parity)", s2)
	}
}

func TestScanRacingPythonInsertsOnce(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	l := e.lead(t, ws, "Acme", 10, "hot")
	// The Python scanner already inserted this period's row under the same deterministic id.
	pyID := signalscan.ScanSignalID(ws, l, "hiring", "moderate|2026-W35", time.Time{})
	if _, err := e.owner.Exec(context.Background(),
		`INSERT INTO signals (id, workspace_id, lead_id, company, signal_type, title, description, source, source_url, weight, created_at, read)
		 VALUES ($1,$2,$3,'Acme','hiring','from python','d','JobSpy','',8,1.0,false)`, pyID, ws, l); err != nil {
		t.Fatal(err)
	}
	e.trigger(t, ws, "tr1", `{"signal_types":["hiring"]}`, "[]")
	e.workbookRow(t, ws, "wb1", l, "{}")
	py := &hiringPy{jobs: map[string]int{"Acme": 8}}
	c, err := signalscan.Run(context.Background(), e.deps(py))
	if err != nil {
		t.Fatal(err)
	}
	if c.SignalsInserted != 0 {
		t.Fatalf("raced insert counted: %+v", c)
	}
	var title string
	var n int
	_ = e.owner.QueryRow(context.Background(), `SELECT count(*), min(title) FROM signals WHERE workspace_id=$1`, ws).Scan(&n, &title)
	if n != 1 || title != "from python" {
		t.Fatalf("rows=%d title=%q", n, title)
	}
	if got := e.count(t, `SELECT count(*) FROM jobs WHERE type='trigger_eval' AND workspace_id=$1`, ws); got != 0 {
		t.Fatalf("on_signal fired for a row Go did not insert: %d jobs", got)
	}
}

func (e *env) trigger(t *testing.T, ws, id, config, scope string) {
	t.Helper()
	if _, err := e.owner.Exec(context.Background(),
		`INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, actions, scope_workbook_ids)
		 VALUES ($1,$2,'n',true,'on_signal',$3::json,'[]'::json,$4::json)`, id, ws, config, scope); err != nil {
		t.Fatal(err)
	}
}

func (e *env) workbookRow(t *testing.T, ws, wb string, lead int64, policy string) int64 {
	t.Helper()
	id := e.prefix + wb
	ctx := context.Background()
	if _, err := e.owner.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id, refresh_policy) VALUES ($1,$1,$2,$3::json) ON CONFLICT DO NOTHING`, id, ws, policy); err != nil {
		t.Fatal(err)
	}
	var rid int64
	if err := e.owner.QueryRow(ctx, `INSERT INTO workbook_rows (workbook_id, workspace_id, lead_id, data) VALUES ($1,$2,$3,'{}'::json) RETURNING id`, id, ws, lead).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	return rid
}

func TestOnSignalOnlyForNewRows(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	l := e.lead(t, ws, "Acme", 10, "hot")
	row := e.workbookRow(t, ws, "wb1", l, "{}")
	e.trigger(t, ws, "tr-hiring", `{"signal_types":["hiring"]}`, "[]")
	e.trigger(t, ws, "tr-funding", `{"signal_types":["funding"]}`, "[]")
	e.trigger(t, ws, "tr-other-wb", `{}`, `["some-other-workbook"]`)
	py := &hiringPy{jobs: map[string]int{"Acme": 8}}
	d := e.deps(py)

	if _, err := signalscan.Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	sid := signalscan.ScanSignalID(ws, l, "hiring", "moderate", fixedNow)
	rows, err := e.owner.Query(context.Background(),
		`SELECT executor, status, priority, fire_key, payload::text FROM jobs WHERE type='trigger_eval' AND workspace_id=$1 ORDER BY id`, ws)
	if err != nil {
		t.Fatal(err)
	}
	type job struct {
		exec, status string
		prio         int
		fk           *string
		payload      string
	}
	var got []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.exec, &j.status, &j.prio, &j.fk, &j.payload); err != nil {
			t.Fatal(err)
		}
		got = append(got, j)
	}
	rows.Close()
	if len(got) != 1 {
		t.Fatalf("want exactly the hiring rule to fire once, got %d: %+v", len(got), got)
	}
	var p struct {
		TriggerID   string `json:"trigger_id"`
		WorkspaceID string `json:"workspace_id"`
		Targets     []struct {
			WorkbookID string `json:"workbook_id"`
			RowID      string `json:"row_id"`
		} `json:"targets"`
		FireSource string `json:"fire_source"`
		FireKey    string `json:"fire_key"`
		DryRun     bool   `json:"dry_run"`
	}
	if err := json.Unmarshal([]byte(got[0].payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.TriggerID != "tr-hiring" || p.WorkspaceID != ws || p.FireSource != "signal" || p.FireKey != "signal:"+sid || p.DryRun ||
		len(p.Targets) != 1 || p.Targets[0].WorkbookID != e.prefix+"wb1" || p.Targets[0].RowID != fmt.Sprint(row) {
		t.Fatalf("payload %s", got[0].payload)
	}
	if got[0].exec != "go" || got[0].status != "pending" || got[0].prio != 1 {
		t.Fatalf("job %+v", got[0])
	}

	// Re-scan: the row already exists, so on_signal must not fire again.
	if _, err := signalscan.Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE type='trigger_eval' AND workspace_id=$1`, ws); n != 1 {
		t.Fatalf("rescan fired on_signal again: %d jobs", n)
	}
}

func TestOneWorkspaceFailureDoesNotAbort(t *testing.T) {
	e := setup(t)
	bad := e.workspace(t, "a-bad")
	good := e.workspace(t, "b-good")
	lb := e.lead(t, bad, "Bad Co", 10, "hot")
	lg := e.lead(t, good, "Good Co", 10, "hot")
	py := &hiringPy{
		jobs:   map[string]int{"Bad Co": 8, "Good Co": 8},
		titles: map[string][]string{"Bad Co": {"nul\x00byte"}}, // Postgres rejects NUL: the insert fails
	}
	c, err := signalscan.Run(context.Background(), e.deps(py))
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspacesFailed < 1 {
		t.Fatalf("failure not counted: %+v", c)
	}
	if n := len(e.signalIDs(t, bad)); n != 0 {
		t.Fatalf("bad workspace wrote %d signals", n)
	}
	if n := len(e.signalIDs(t, good)); n != 1 {
		t.Fatalf("good workspace wrote %d signals, want 1", n)
	}
	if s, _ := e.score(t, bad, lb); s != 10 {
		t.Fatalf("failed workspace was boosted: %d", s)
	}
	if s, _ := e.score(t, good, lg); s != 18 {
		t.Fatalf("good workspace score %d, want 18", s)
	}
}

func (e *env) jobsOf(t *testing.T, typ string) []struct {
	ws, exec, payload string
	prio              int
} {
	t.Helper()
	rows, err := e.owner.Query(context.Background(), `SELECT coalesce(workspace_id,''), executor, payload::text, priority FROM jobs WHERE type=$1 AND (workspace_id LIKE $2 OR (workspace_id IS NULL AND fire_key LIKE 'signal_scan:2026-08-27T%')) ORDER BY id`, typ, e.prefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct {
		ws, exec, payload string
		prio              int
	}
	for rows.Next() {
		var j struct {
			ws, exec, payload string
			prio              int
		}
		if err := rows.Scan(&j.ws, &j.exec, &j.payload, &j.prio); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func TestTriggerPassScopedPerWorkspace(t *testing.T) {
	e := setup(t)
	a := e.workspace(t, "a")
	b := e.workspace(t, "b")
	e.workbookRow(t, a, "wa-hit", 0, `{"enabled":true,"on_signal":["hiring","news"]}`)
	e.workbookRow(t, a, "wa-miss", 0, `{"enabled":true,"on_signal":["funding"]}`)
	e.workbookRow(t, a, "wa-off", 0, `{"enabled":false,"on_signal":["hiring"]}`)
	e.workbookRow(t, b, "wb-hit", 0, `{"enabled":true,"on_signal":["hiring"]}`)
	e.workbookRow(t, b, "wb-none", 0, `{}`)

	d := e.deps(&hiringPy{})
	d.RefreshExecutor = func(string) string { return "go" }
	d.ScanExecutor = func() string { return "go" }
	claim := &queue.Claim{ID: 1, Type: "signal_scan", Payload: json.RawMessage(`{"interval_minutes": 60, "signal_types": ["hiring", "news"]}`)}
	if err := signalscan.HandleScan(context.Background(), d, claim); err != nil {
		t.Fatal(err)
	}

	jobs := e.jobsOf(t, "refresh_workbook")
	if len(jobs) != 2 {
		t.Fatalf("refresh jobs: %+v", jobs)
	}
	byWS := map[string]string{}
	for _, j := range jobs {
		byWS[j.ws] = j.payload
		if j.prio != 2 {
			t.Errorf("priority %d, want 2", j.prio)
		}
		if !strings.Contains(j.payload, `"reason": "signal"`) && !strings.Contains(j.payload, `"reason":"signal"`) {
			t.Errorf("payload %s", j.payload)
		}
	}
	if !strings.Contains(byWS[a], e.prefix+"wa-hit") || !strings.Contains(byWS[b], e.prefix+"wb-hit") {
		t.Fatalf("jobs landed in the wrong workspace: %v", byWS)
	}
	for _, j := range jobs {
		if j.exec != "go" {
			t.Errorf("executor %s for %s", j.exec, j.ws)
		}
	}
	// activity row is tenant-stamped and names the matching types (sorted)
	var msg, aws string
	if err := e.owner.QueryRow(context.Background(), `SELECT message, workspace_id FROM workbook_activity WHERE workbook_id=$1`, e.prefix+"wa-hit").Scan(&msg, &aws); err != nil {
		t.Fatal(err)
	}
	if msg != "signal trigger → refresh (hiring, news)" || aws != a {
		t.Fatalf("activity %q in %q", msg, aws)
	}
	if n := e.count(t, `SELECT count(*) FROM workbook_activity WHERE workspace_id LIKE $1`, e.prefix+"%"); n != 2 {
		t.Fatalf("activity rows %d", n)
	}

	// the next periodic scan is re-enqueued, global (no workspace), delayed by the interval
	next := e.jobsOf(t, "signal_scan")
	if len(next) != 1 || next[0].ws != "" || next[0].exec != "go" || !strings.Contains(next[0].payload, `"interval_minutes": 60`) && !strings.Contains(next[0].payload, `"interval_minutes":60`) {
		t.Fatalf("next scan %+v", next)
	}
	var fk string
	var runAt time.Time
	if err := e.owner.QueryRow(context.Background(), `SELECT fire_key, next_run_at FROM jobs WHERE type='signal_scan' AND workspace_id IS NULL AND status='pending' ORDER BY id DESC LIMIT 1`).Scan(&fk, &runAt); err != nil {
		t.Fatal(err)
	}
	if fk != "signal_scan:2026-08-27T14:00:00+00:00" || !runAt.Equal(time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("fire_key %q run at %v", fk, runAt)
	}
	// single flight: the same occurrence is not queued twice
	if err := signalscan.HandleScan(context.Background(), d, claim); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE type='signal_scan' AND fire_key=$1 AND status='pending'`, fk); n != 1 {
		t.Fatalf("duplicate scan job: %d", n)
	}
}
