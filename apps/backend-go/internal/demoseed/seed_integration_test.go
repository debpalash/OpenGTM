//go:build integration

package demoseed_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/demoseed"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const wsID = "ws-demoseed-test"

type fakeRunner struct {
	calls []demoseed.RunTarget
	err   error
}

func (f *fakeRunner) StartRun(_ context.Context, t demoseed.RunTarget) error {
	f.calls = append(f.calls, t)
	return f.err
}

func setup(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	own, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(own.Close)
	clean := func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM jobs WHERE type = 'run_workbook' AND payload::text LIKE '%demo-zero-key-firstrun%'`,
			`DELETE FROM workbook_rows WHERE workbook_id = 'demo-zero-key-firstrun'`,
			`DELETE FROM workbooks WHERE id = 'demo-zero-key-firstrun'`,
			`DELETE FROM workspaces WHERE id = '` + wsID + `'`,
			`DELETE FROM users WHERE username = 'demoseed-owner'`,
		} {
			_, _ = own.Exec(ctx, q)
		}
	}
	clean()
	t.Cleanup(clean)
	return pool, own
}

func mkWorkspace(t *testing.T, own *pgxpool.Pool, slug string, withOwner bool) {
	t.Helper()
	ctx := context.Background()
	owner := any(nil)
	if withOwner {
		var id int
		if err := own.QueryRow(ctx, `INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ('demoseed-owner','x',true,true,'admin') RETURNING id`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		owner = id
	}
	if _, err := own.Exec(ctx, `INSERT INTO workspaces (id, name, slug, description, icon, owner_id, created_at, updated_at) VALUES ($1,'Demo',$2,'','x',$3,0,0)`, wsID, slug, owner); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, own *pgxpool.Pool, sql string) (n int) {
	t.Helper()
	if err := own.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSeedWritesWhatPythonWrote(t *testing.T) {
	pool, own := setup(t)
	mkWorkspace(t, own, "demoseed-test", true)
	ctx := context.Background()
	run := &fakeRunner{}
	res, err := demoseed.Seed(ctx, pool, demoseed.Options{WorkspaceSlug: "demoseed-test", Runner: run})
	if err != nil || !res.Created || !res.RunStarted {
		t.Fatalf("seed: %+v %v", res, err)
	}
	v := load(t)
	wb := v.Workbooks[0]
	var name, desc, status, src, cols string
	var total, done int
	var sync bool
	var bmax, bspent float64
	var ws string
	if err := own.QueryRow(ctx, `SELECT name, description, status, source_type, columns_config::text, total_rows, completed_rows, sync_to_leads,
	   budget_max_usd, budget_spent_usd, workspace_id FROM workbooks WHERE id = $1`, wb.ID).Scan(&name, &desc, &status, &src, &cols, &total, &done, &sync, &bmax, &bspent, &ws); err != nil {
		t.Fatal(err)
	}
	// Python set status "running" when it enqueued the run; here the run is started through the API
	// (which flips the status), so the seed leaves "draft".
	if name != wb.Name || desc != wb.Description || status != "draft" || src != wb.SourceType || total != wb.TotalRows ||
		done != 0 || sync || bmax != wb.BudgetMax || bspent != wb.BudgetSpent || ws != wsID {
		t.Fatalf("workbook row: %q %q %q %q %d %d %v %v %v %q", name, desc, status, src, total, done, sync, bmax, bspent, ws)
	}
	if !reflect.DeepEqual(canon(t, []byte(cols)), canon(t, wb.ColumnsConfig)) {
		t.Fatalf("columns_config differs: %s", cols)
	}
	rows, err := own.Query(ctx, `SELECT position, data::text, enrichments::text, workspace_id FROM workbook_rows WHERE workbook_id = $1 ORDER BY position`, wb.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var pos int
		var data, enr, rws string
		if err := rows.Scan(&pos, &data, &enr, &rws); err != nil {
			t.Fatal(err)
		}
		want := v.Rows[i]
		var got map[string]string
		_ = json.Unmarshal([]byte(data), &got)
		if pos != want.Position || !reflect.DeepEqual(got, want.Data) || canon(t, []byte(enr)).(map[string]any) == nil || len(canon(t, []byte(enr)).(map[string]any)) != 0 || rws != wsID {
			t.Fatalf("row %d: %d %s %s %s", i, pos, data, enr, rws)
		}
		i++
	}
	if i != len(v.Rows) {
		t.Fatalf("%d rows, Python wrote %d", i, len(v.Rows))
	}
	if len(run.calls) != 1 || run.calls[0].WorkspaceID != wsID || run.calls[0].OwnerUsername != "demoseed-owner" ||
		run.calls[0].WorkbookID != wb.ID || !reflect.DeepEqual(run.calls[0].ColumnIDs, v.Jobs[0].Payload.ColumnIDs) {
		t.Fatalf("run target = %+v", run.calls)
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	pool, own := setup(t)
	mkWorkspace(t, own, "demoseed-test", true)
	ctx := context.Background()
	run := &fakeRunner{}
	if _, err := demoseed.Seed(ctx, pool, demoseed.Options{WorkspaceSlug: "demoseed-test", Runner: run}); err != nil {
		t.Fatal(err)
	}
	// the run the first pass started is queued (as POST /run would have): a rerun neither reseeds nor re-runs
	if _, err := own.Exec(ctx, `UPDATE workbooks SET status = 'running' WHERE id = 'demo-zero-key-firstrun'`); err != nil {
		t.Fatal(err)
	}
	res, err := demoseed.Seed(ctx, pool, demoseed.Options{WorkspaceSlug: "demoseed-test", Runner: run})
	if err != nil || res.Created || res.RunStarted {
		t.Fatalf("rerun: %+v %v", res, err)
	}
	if n := count(t, own, `SELECT count(*) FROM workbooks WHERE id = 'demo-zero-key-firstrun'`); n != 1 {
		t.Fatalf("%d workbooks", n)
	}
	if n := count(t, own, `SELECT count(*) FROM workbook_rows WHERE workbook_id = 'demo-zero-key-firstrun'`); n != 25 {
		t.Fatalf("%d rows", n)
	}
	if len(run.calls) != 1 {
		t.Fatalf("runner called %d times", len(run.calls))
	}
	// SEED_FORCE_RUN=1 runs again (Python: SEED_FORCE_RUN) unless a run is already queued
	res, err = demoseed.Seed(ctx, pool, demoseed.Options{WorkspaceSlug: "demoseed-test", Runner: run, Force: true})
	if err != nil || !res.RunStarted || len(run.calls) != 2 {
		t.Fatalf("force: %+v %v calls=%d", res, err, len(run.calls))
	}
	if _, err := own.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, next_run_at, retry_count, max_retries, executor)
	    VALUES ('run_workbook', '{"workbook_id": "demo-zero-key-firstrun"}', 'pending', 1, now(), now(), 0, 3, 'python')`); err != nil {
		t.Fatal(err)
	}
	if res, err = demoseed.Seed(ctx, pool, demoseed.Options{WorkspaceSlug: "demoseed-test", Runner: run, Force: true}); err != nil || res.RunStarted || len(run.calls) != 2 {
		t.Fatalf("a queued run blocks another: %+v %v calls=%d", res, err, len(run.calls))
	}
}

func TestSeedRetriesRunThatCouldNotStart(t *testing.T) {
	pool, own := setup(t)
	mkWorkspace(t, own, "demoseed-test", true)
	ctx := context.Background()
	run := &fakeRunner{err: errors.New("api not up")}
	res, err := demoseed.Seed(ctx, pool, demoseed.Options{WorkspaceSlug: "demoseed-test", Runner: run})
	if err != nil || !res.Created || res.RunStarted || res.RunErr == nil {
		t.Fatalf("a failed run start must not fail the seed: %+v %v", res, err)
	}
	run.err = nil
	res, err = demoseed.Seed(ctx, pool, demoseed.Options{WorkspaceSlug: "demoseed-test", Runner: run})
	if err != nil || res.Created || !res.RunStarted {
		t.Fatalf("the rerun starts the run that never started: %+v %v", res, err)
	}
}

func TestSeedNeverCreatesUsersAndWaitsForSetup(t *testing.T) {
	pool, own := setup(t)
	ctx := context.Background()
	users := count(t, own, `SELECT count(*) FROM users`)
	_, err := demoseed.Seed(ctx, pool, demoseed.Options{WorkspaceSlug: "demoseed-test", Runner: &fakeRunner{}})
	if !errors.Is(err, demoseed.ErrNoWorkspace) {
		t.Fatalf("no workspace yet must be ErrNoWorkspace, got %v", err)
	}
	if n := count(t, own, `SELECT count(*) FROM users`); n != users {
		t.Fatalf("seed created users: %d -> %d", users, n)
	}
	if n := count(t, own, `SELECT count(*) FROM workbooks WHERE id = 'demo-zero-key-firstrun'`); n != 0 {
		t.Fatalf("workbook written without a workspace")
	}
	// a workspace without an owner is seeded; only the run is skipped (no identity to act as)
	mkWorkspace(t, own, "demoseed-test", false)
	run := &fakeRunner{}
	res, err := demoseed.Seed(ctx, pool, demoseed.Options{WorkspaceSlug: "demoseed-test", Runner: run})
	if err != nil || !res.Created || res.RunStarted || res.RunErr == nil || len(run.calls) != 0 {
		t.Fatalf("ownerless workspace: %+v %v", res, err)
	}
}

func TestAPIRunnerCallsRunRouteAsOwner(t *testing.T) {
	secret := []byte("test-secret-key-0123456789abcdef0123")
	signer := auth.Signer{Secret: secret, AccessTTL: 5 * time.Minute}
	var gotPath, gotWS string
	var gotBody map[string]any
	var claims auth.Claims
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotWS = r.URL.Path, r.Header.Get("X-Workspace-Id")
		tok := r.Header.Get("Authorization")[len("Bearer "):]
		claims, _ = signer.ParseAccess(tok)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"status":"started"}`))
	}))
	defer srv.Close()
	r := demoseed.APIRunner{BaseURL: srv.URL, Signer: signer}
	err := r.StartRun(context.Background(), demoseed.RunTarget{WorkspaceID: "w1", OwnerUsername: "alice", WorkbookID: "wb", ColumnIDs: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/workbooks/wb/run" || gotWS != "w1" || claims.Sub != "alice" {
		t.Fatalf("request: %s ws=%q sub=%q", gotPath, gotWS, claims.Sub)
	}
	if cols, _ := gotBody["column_ids"].([]any); len(cols) != 2 || gotBody["fill_missing"] != false {
		t.Fatalf("body = %v", gotBody)
	}
	// non-2xx and an unreachable API are errors the seed reports without failing
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 402) }))
	bad.Close()
	if err := (demoseed.APIRunner{BaseURL: bad.URL, Signer: signer}).StartRun(context.Background(), demoseed.RunTarget{WorkspaceID: "w1", OwnerUsername: "alice", WorkbookID: "wb"}); err == nil {
		t.Fatal("unreachable API must be an error")
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, `{"detail":"x"}`, 402) }))
	defer srv2.Close()
	if err := (demoseed.APIRunner{BaseURL: srv2.URL, Signer: signer}).StartRun(context.Background(), demoseed.RunTarget{WorkspaceID: "w1", OwnerUsername: "alice", WorkbookID: "wb"}); err == nil {
		t.Fatal("HTTP 402 must be an error")
	}
}
