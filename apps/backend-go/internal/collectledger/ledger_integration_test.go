//go:build integration

package collectledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	wsA = "ws-t10-a"
	wsB = "ws-t10-b"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 123456000, time.UTC)

func setup(t *testing.T, conns int32) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, conns)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM collection_jobs`) // cascades stages
				return err
			})
		}
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE fire_key LIKE 'collect:ws-t10-%'`)
	}
	clean()
	t.Cleanup(clean)
	advanceStageSeq(t) // sequence-drawn stage ids stay above other packages' explicit low ids
	return pool, ctx
}

// advanceStageSeq moves the stage id sequence to GREATEST(1000, max(id), current): never backwards.
func advanceStageSeq(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, `SELECT setval(pg_get_serial_sequence('collection_job_stages','id'),
		GREATEST(1000, (SELECT COALESCE(max(id), 0) FROM collection_job_stages), (SELECT last_value FROM collection_job_stages_id_seq)))`); err != nil {
		t.Fatal(err)
	}
}

func in(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, ws, fn); err != nil {
		t.Fatal(err)
	}
}

// seed inserts a job with explicit state (raw SQL; the store has no such writer).
func seed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id, status, createdAt string) {
	t.Helper()
	in(t, ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO collection_jobs (id, workspace_id, query, status, created_at, attempts, error, completed_at)
			VALUES ($1,$2,'q',$3,$4,2,'old err','2026-01-01T00:00:00+00:00')`, id, ws, status, createdAt)
		return err
	})
}

func seedStage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, job, stage, details string) {
	t.Helper()
	in(t, ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO collection_job_stages (workspace_id, job_id, stage, status, details, started_at)
			VALUES ($1,$2,$3,'done',$4,'2026-09-01T12:00:00+00:00')`, ws, job, stage, details)
		return err
	})
}

type row struct {
	Status, Error, CompletedAt string
	Attempts                   int
}

func get(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id string) *row {
	t.Helper()
	var r *row
	in(t, ctx, pool, ws, func(tx pgx.Tx) error {
		var x row
		err := tx.QueryRow(ctx, `SELECT status, error, completed_at, attempts FROM collection_jobs WHERE id=$1`, id).
			Scan(&x.Status, &x.Error, &x.CompletedAt, &x.Attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		r = &x
		return err
	})
	return r
}

func TestCreateIdempotent(t *testing.T) {
	pool, ctx := setup(t, 4)
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if err := collectledger.Create(ctx, tx, wsA, "t10-c1", "first", "market_search", `{"a":1}`, t0); err != nil {
			t.Fatal(err)
		}
		if err := collectledger.Create(ctx, tx, wsA, "t10-c1", "second", "other", `{}`, t0.Add(time.Hour)); err != nil {
			t.Fatalf("second create must be a no-op, got %v", err)
		}
		return nil
	})
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		d, err := collectledger.Detail(ctx, tx, "t10-c1")
		if err != nil {
			t.Fatal(err)
		}
		if d.Query != "first" || d.Intent != "market_search" || d.Status != "pending" || d.Tier != 1 || d.MaxAttempts != 3 ||
			d.CreatedAt != "2026-09-01T12:00:00.123456+00:00" || d.WorkspaceID != wsA || d.StartedAt != "" || d.CompletedAt != "" {
			t.Fatalf("unexpected job %+v", d)
		}
		return nil
	})
}

func TestDetailParsesIntentAndStageDetails(t *testing.T) {
	pool, ctx := setup(t, 4)
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		return collectledger.Create(ctx, tx, wsA, "t10-d1", "q", "market_search", `{"z":1,"a":[1,2.0,"é"]}`, t0)
	})
	seedStage(t, ctx, pool, wsA, "t10-d1", "validate", `{"note":"東京","n":3}`)
	seedStage(t, ctx, pool, wsA, "t10-d1", "dedup", `[1,2]`)
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		d, err := collectledger.Detail(ctx, tx, "t10-d1")
		if err != nil {
			t.Fatal(err)
		}
		if string(d.IntentDetails) != `{"z":1,"a":[1,2.0,"é"]}` {
			t.Errorf("intent_details %s", d.IntentDetails)
		}
		if len(d.Stages) != 2 || d.Stages[0].Stage != "validate" || d.Stages[1].Stage != "dedup" || d.Stages[0].ID >= d.Stages[1].ID {
			t.Fatalf("stages %+v", d.Stages)
		}
		if string(d.Stages[0].Details) != `{"note":"東京","n":3}` || string(d.Stages[1].Details) != `[1,2]` {
			t.Errorf("details %s %s", d.Stages[0].Details, d.Stages[1].Details)
		}
		st, err := collectledger.Stages(ctx, tx, "t10-d1")
		if err != nil || len(st) != 2 || !reflect.DeepEqual(st, d.Stages) {
			t.Errorf("Stages: %v %+v", err, st)
		}
		if _, err := collectledger.Detail(ctx, tx, "nope"); !errors.Is(err, collectledger.ErrNotFound) {
			t.Errorf("missing detail: %v", err)
		}
		if _, err := collectledger.Stages(ctx, tx, "nope"); !errors.Is(err, collectledger.ErrNotFound) {
			t.Errorf("missing stages: %v", err)
		}
		return nil
	})
}

func TestInvalidDetailsJSONBecomesEmptyObjectInStore(t *testing.T) {
	pool, ctx := setup(t, 4)
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		return collectledger.Create(ctx, tx, wsA, "t10-i1", "q", "market_search", `not json{`, t0)
	})
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		return collectledger.Create(ctx, tx, wsA, "t10-i2", "q", "market_search", `NaN`, t0)
	})
	seedStage(t, ctx, pool, wsA, "t10-i1", "s1", ``)
	seedStage(t, ctx, pool, wsA, "t10-i1", "s2", `{bad`)
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		d, err := collectledger.Detail(ctx, tx, "t10-i1")
		if err != nil {
			t.Fatal(err)
		}
		if string(d.IntentDetails) != `{}` || string(d.Stages[0].Details) != `{}` || string(d.Stages[1].Details) != `{}` {
			t.Errorf("%s %s %s", d.IntentDetails, d.Stages[0].Details, d.Stages[1].Details)
		}
		// NaN parses in Python but the response render fails: surfaced as an error, not {}.
		if _, err := collectledger.Detail(ctx, tx, "t10-i2"); !errors.Is(err, collectledger.ErrNotRenderable) {
			t.Errorf("NaN detail: %v", err)
		}
		return nil
	})
}

func TestJobJSONMatchesFixtures(t *testing.T) {
	pool, ctx := setup(t, 4)
	read := func(name string) any {
		raw, err := os.ReadFile("../../testdata/contract/jobs/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var f struct{ Response struct{ Body any } }
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		return f.Response.Body
	}
	canon := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	// list row with an unparsable raw intent_details string, as stored.
	list := read("jobs_list_status_failed.json").([]any)
	r := list[0].(map[string]any)
	r["workspace_id"] = wsA // recorded under ws-alpha; the test tenant differs
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO collection_jobs (id, workspace_id, query, intent, intent_details, status, tier, attempts, max_attempts,
			leads_found, proxy_used, error, created_at, started_at, completed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			r["id"], wsA, r["query"], r["intent"], r["intent_details"], r["status"], int(r["tier"].(float64)), int(r["attempts"].(float64)),
			int(r["max_attempts"].(float64)), int(r["leads_found"].(float64)), r["proxy_used"], r["error"], r["created_at"], r["started_at"], r["completed_at"])
		return err
	})
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		jobs, err := collectledger.List(ctx, tx, "failed", 50)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("list: %v %v", err, jobs)
		}
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(jobs); err != nil {
			t.Fatal(err)
		}
		var back any
		_ = json.Unmarshal([]byte(buf.String()), &back)
		if canon(back) != canon(list) {
			t.Errorf("list body differs:\n got %s\nwant %s", canon(back), canon(list))
		}
		// recorded key order (README Plan 14): columns in ledger order, workspace_id last.
		var rows []json.RawMessage
		if err := json.Unmarshal([]byte(buf.String()), &rows); err != nil {
			t.Fatal(err)
		}
		keys := objKeys(t, rows[0])
		want := "id,query,intent,intent_details,status,tier,attempts,max_attempts,leads_found,proxy_used,error,created_at,started_at,completed_at,workspace_id"
		if strings.Join(keys, ",") != want {
			t.Errorf("job key order %v", keys)
		}
		return nil
	})

	// detail: job columns then stages, parsed JSON, unicode intact.
	det := read("job_detail_done_unicode.json").(map[string]any)
	det["workspace_id"] = wsA
	// The fixture's stage ids (7, 8) collide with api tests' explicit ids 1..N: use a disjoint range.
	for _, st := range det["stages"].([]any) {
		sm := st.(map[string]any)
		sm["id"] = sm["id"].(float64) + 5_000_000
	}
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		iraw, _ := json.Marshal(det["intent_details"])
		_, err := tx.Exec(ctx, `INSERT INTO collection_jobs (id, workspace_id, query, intent, intent_details, status, tier, attempts, max_attempts,
			leads_found, proxy_used, error, created_at, started_at, completed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			det["id"], wsA, det["query"], det["intent"], string(iraw), det["status"], int(det["tier"].(float64)), int(det["attempts"].(float64)),
			int(det["max_attempts"].(float64)), int(det["leads_found"].(float64)), det["proxy_used"], det["error"], det["created_at"], det["started_at"], det["completed_at"])
		if err != nil {
			return err
		}
		for _, s := range det["stages"].([]any) {
			sm := s.(map[string]any)
			draw, _ := json.Marshal(sm["details"])
			if _, err := tx.Exec(ctx, `INSERT INTO collection_job_stages (id, workspace_id, job_id, stage, status, input_count, output_count,
				rejected_count, details, started_at, completed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
				int64(sm["id"].(float64)), wsA, sm["job_id"], sm["stage"], sm["status"], int(sm["input_count"].(float64)),
				int(sm["output_count"].(float64)), int(sm["rejected_count"].(float64)), string(draw), sm["started_at"], sm["completed_at"]); err != nil {
				return err
			}
		}
		return nil
	})
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		d, err := collectledger.Detail(ctx, tx, det["id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(d)
		var back any
		_ = json.Unmarshal([]byte(buf.String()), &back)
		if canon(back) != canon(det) {
			t.Errorf("detail body differs:\n got %s\nwant %s", canon(back), canon(det))
		}
		keys := objKeys(t, []byte(buf.String()))
		if keys[len(keys)-1] != "stages" || keys[len(keys)-2] != "workspace_id" {
			t.Errorf("detail key order %v", keys)
		}
		var parts struct{ Stages []json.RawMessage }
		_ = json.Unmarshal([]byte(buf.String()), &parts)
		sk := objKeys(t, parts.Stages[0])
		if strings.Join(sk, ",") != "id,job_id,stage,status,input_count,output_count,rejected_count,details,started_at,completed_at" {
			t.Errorf("stage key order %v", sk)
		}
		return nil
	})
}

// objKeys returns the keys of one JSON object in document order.
func objKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("objKeys: not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestCancelOnlyFromRunningOrPending(t *testing.T) {
	pool, ctx := setup(t, 4)
	for st, want := range map[string]bool{"running": true, "pending": true, "done": false, "failed": false, "cancelled": false} {
		id := "t10-cx-" + st
		seed(t, ctx, pool, wsA, id, st, "2026-09-01T00:00:00+00:00")
		in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			changed, err := collectledger.Cancel(ctx, tx, id, t0)
			if err != nil || changed != want {
				t.Errorf("%s: changed=%v err=%v want %v", st, changed, err, want)
			}
			return nil
		})
		r := get(t, ctx, pool, wsA, id)
		if want {
			if r.Status != "cancelled" || r.Error != "Cancelled by user" || r.CompletedAt != "2026-09-01T12:00:00.123456+00:00" {
				t.Errorf("%s: %+v", st, r)
			}
		} else if r.Status != st || r.Error != "old err" || r.CompletedAt != "2026-01-01T00:00:00+00:00" {
			t.Errorf("%s must be untouched: %+v", st, r)
		}
	}
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if changed, err := collectledger.Cancel(ctx, tx, "t10-missing", t0); err != nil || changed {
			t.Errorf("missing: %v %v", changed, err)
		}
		return nil
	})
}

func TestRetryOnlyFromFailedOrCancelled(t *testing.T) {
	pool, ctx := setup(t, 4)
	for st, want := range map[string]bool{"failed": true, "cancelled": true, "running": false, "pending": false, "done": false} {
		id := "t10-rt-" + st
		seed(t, ctx, pool, wsA, id, st, "2026-09-01T00:00:00+00:00")
		in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			changed, err := collectledger.Retry(ctx, tx, id)
			if err != nil || changed != want {
				t.Errorf("%s: changed=%v err=%v want %v", st, changed, err, want)
			}
			return nil
		})
		r := get(t, ctx, pool, wsA, id)
		if want {
			if r.Status != "pending" || r.Error != "" || r.CompletedAt != "" || r.Attempts != 0 {
				t.Errorf("%s: %+v", st, r)
			}
		} else if r.Status != st || r.Error != "old err" || r.Attempts != 2 {
			t.Errorf("%s must be untouched: %+v", st, r)
		}
	}
}

func TestMarkEnqueueFailed(t *testing.T) {
	pool, ctx := setup(t, 4)
	seed(t, ctx, pool, wsA, "t10-mf", "pending", "2026-09-01T00:00:00+00:00")
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		return collectledger.MarkEnqueueFailed(ctx, tx, "t10-mf", "Queue enqueue failed: boom", t0)
	})
	if r := get(t, ctx, pool, wsA, "t10-mf"); r.Status != "failed" || r.Error != "Queue enqueue failed: boom" || r.CompletedAt != "2026-09-01T12:00:00.123456+00:00" {
		t.Errorf("%+v", r)
	}
}

func TestDeleteCascadesStages(t *testing.T) {
	pool, ctx := setup(t, 4)
	seed(t, ctx, pool, wsA, "t10-del", "done", "2026-09-01T00:00:00+00:00")
	seed(t, ctx, pool, wsA, "t10-keep", "done", "2026-09-01T00:00:00+00:00")
	seedStage(t, ctx, pool, wsA, "t10-del", "s", `{}`)
	seedStage(t, ctx, pool, wsA, "t10-keep", "s", `{}`)
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if err := collectledger.Delete(ctx, tx, "t10-del"); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM collection_job_stages WHERE job_id='t10-del'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("stages left: %d %v", n, err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM collection_job_stages WHERE job_id='t10-keep'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("other job's stages: %d %v", n, err)
		}
		if err := collectledger.Delete(ctx, tx, "t10-del"); err != nil {
			t.Errorf("deleting a missing job is a no-op: %v", err)
		}
		return nil
	})
	if get(t, ctx, pool, wsA, "t10-del") != nil || get(t, ctx, pool, wsA, "t10-keep") == nil {
		t.Error("wrong rows deleted")
	}
}

func TestListStatusFilterAndLimit(t *testing.T) {
	pool, ctx := setup(t, 4)
	seed(t, ctx, pool, wsA, "t10-l1", "done", "2026-09-01T10:00:00+00:00")
	seed(t, ctx, pool, wsA, "t10-l2", "failed", "2026-09-01T11:00:00+00:00")
	seed(t, ctx, pool, wsA, "t10-l3", "done", "2026-09-01T12:00:00+00:00")
	seed(t, ctx, pool, wsA, "t10-l4", "done", "2026-09-01T12:00:00+00:00") // same created_at: id DESC tiebreak
	ids := func(js []collectledger.Job) []string {
		out := []string{}
		for _, j := range js {
			out = append(out, j.ID)
		}
		return out
	}
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		all, err := collectledger.List(ctx, tx, "", 50)
		if err != nil || !reflect.DeepEqual(ids(all), []string{"t10-l4", "t10-l3", "t10-l2", "t10-l1"}) {
			t.Errorf("all: %v %v", ids(all), err)
		}
		done, _ := collectledger.List(ctx, tx, "done", 50)
		if !reflect.DeepEqual(ids(done), []string{"t10-l4", "t10-l3", "t10-l1"}) {
			t.Errorf("done: %v", ids(done))
		}
		two, _ := collectledger.List(ctx, tx, "", 2)
		if !reflect.DeepEqual(ids(two), []string{"t10-l4", "t10-l3"}) {
			t.Errorf("limit 2: %v", ids(two))
		}
		none, err := collectledger.List(ctx, tx, "bogus", 50)
		if err != nil || none == nil || len(none) != 0 {
			t.Errorf("unknown status must be an empty non-nil slice: %#v %v", none, err)
		}
		if b, _ := json.Marshal(none); string(b) != "[]" {
			t.Errorf("empty list marshals %s", b)
		}
		return nil
	})
	// default limit is 50 (limit <= 0).
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO collection_jobs (id, workspace_id, query, created_at)
			SELECT 't10-bulk-' || g, $1, 'q', '2026-08-01T00:00:00+00:00' FROM generate_series(1,60) g`, wsA)
		return err
	})
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		js, err := collectledger.List(ctx, tx, "", 0)
		if err != nil || len(js) != 50 {
			t.Errorf("default limit: %d %v", len(js), err)
		}
		return nil
	})
}

func TestLedgerTenantIsolation(t *testing.T) {
	pool, ctx := setup(t, 1) // one connection alternating tenants: a leaked GUC would cross
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		return collectledger.Create(ctx, tx, wsA, "t10-iso", "secret", "market_search", `{}`, t0)
	})
	seedStage(t, ctx, pool, wsA, "t10-iso", "s", `{}`)
	in(t, ctx, pool, wsB, func(tx pgx.Tx) error {
		if _, err := collectledger.Detail(ctx, tx, "t10-iso"); !errors.Is(err, collectledger.ErrNotFound) {
			t.Errorf("B detail: %v", err)
		}
		if _, err := collectledger.Stages(ctx, tx, "t10-iso"); !errors.Is(err, collectledger.ErrNotFound) {
			t.Errorf("B stages: %v", err)
		}
		if js, err := collectledger.List(ctx, tx, "", 50); err != nil || len(js) != 0 {
			t.Errorf("B list: %v %v", js, err)
		}
		if c, err := collectledger.Cancel(ctx, tx, "t10-iso", t0); err != nil || c {
			t.Errorf("B cancel: %v %v", c, err)
		}
		if c, err := collectledger.Retry(ctx, tx, "t10-iso"); err != nil || c {
			t.Errorf("B retry: %v %v", c, err)
		}
		if err := collectledger.MarkEnqueueFailed(ctx, tx, "t10-iso", "x", t0); err != nil {
			t.Errorf("B mark: %v", err)
		}
		if err := collectledger.Delete(ctx, tx, "t10-iso"); err != nil {
			t.Errorf("B delete: %v", err)
		}
		// workspace must equal the transaction's tenant.
		if err := collectledger.Create(ctx, tx, wsA, "t10-evil", "q", "market_search", `{}`, t0); !errors.Is(err, collectledger.ErrTenantMismatch) {
			t.Errorf("cross-tenant create: %v", err)
		}
		if _, err := collectledger.CancelQueueJob(ctx, tx, wsA, "t10-iso", t0); !errors.Is(err, collectledger.ErrTenantMismatch) {
			t.Errorf("cross-tenant queue cancel: %v", err)
		}
		// B can use the same id for its own job without touching A's.
		return collectledger.Create(ctx, tx, wsB, "t10-iso-b", "mine", "market_search", `{}`, t0)
	})
	if r := get(t, ctx, pool, wsA, "t10-iso"); r == nil || r.Status != "pending" || r.Error != "" {
		t.Fatalf("A's job was touched: %+v", r)
	}
	in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		d, err := collectledger.Detail(ctx, tx, "t10-iso")
		if err != nil || len(d.Stages) != 1 {
			t.Errorf("A detail: %v %+v", err, d)
		}
		if js, _ := collectledger.List(ctx, tx, "", 50); len(js) != 1 {
			t.Errorf("A list sees %d", len(js))
		}
		return nil
	})
}

func TestCancelQueueJobOnlyActiveRows(t *testing.T) {
	pool, ctx := setup(t, 4)
	ins := func(fireKey, status string) int {
		var id int
		err := pool.QueryRow(ctx, `INSERT INTO jobs (type, status, priority, created_at, retry_count, max_retries, fire_key, workspace_id, worker_id, locked_at, error)
			VALUES ('collect', $1, 1, now() AT TIME ZONE 'UTC', 0, 3, $2, $3, 'w1', now() AT TIME ZONE 'UTC', 'prev') RETURNING id`,
			status, fireKey, wsA).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pending := ins("collect:"+wsA+":q1", "pending")
	completed := ins("collect:"+wsA+":q1", "completed") // same fire_key, inactive: untouched
	processing := ins("collect:"+wsA+":q2", "processing")
	other := ins("collect:"+wsB+":q1", "pending") // other workspace's key
	type q struct {
		Status, Error     string
		Worker            *string
		Locked, Completed *time.Time
	}
	read := func(id int) q {
		var x q
		var e *string
		if err := pool.QueryRow(ctx, `SELECT status, error, worker_id, locked_at, completed_at FROM jobs WHERE id=$1`, id).
			Scan(&x.Status, &e, &x.Worker, &x.Locked, &x.Completed); err != nil {
			t.Fatal(err)
		}
		if e != nil {
			x.Error = *e
		}
		return x
	}
	var n int
	in(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		n, err = collectledger.CancelQueueJob(ctx, tx, wsA, "q1", t0)
		return err
	})
	if n != 1 {
		t.Fatalf("changed %d want 1", n)
	}
	p := read(pending)
	if p.Status != "cancelled" || p.Error != "Cancelled by user" || p.Worker != nil || p.Locked != nil || p.Completed == nil ||
		!p.Completed.Equal(time.Date(2026, 9, 1, 12, 0, 0, 123456000, time.UTC)) {
		t.Errorf("pending row: %+v completed=%v", p, p.Completed)
	}
	if c := read(completed); c.Status != "completed" || c.Error != "prev" || c.Worker == nil {
		t.Errorf("completed row touched: %+v", c)
	}
	if o := read(other); o.Status != "pending" || o.Worker == nil {
		t.Errorf("other workspace's row touched: %+v", o)
	}
	in(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		n, err = collectledger.CancelQueueJob(ctx, tx, wsA, "q2", t0)
		return err
	})
	if n != 1 || read(processing).Status != "cancelled" {
		t.Errorf("processing: %d %+v", n, read(processing))
	}
	in(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		n, err = collectledger.CancelQueueJob(ctx, tx, wsA, "q2", t0) // already cancelled
		return err
	})
	if n != 0 {
		t.Errorf("repeat cancel changed %d", n)
	}
}

func TestReconcileRetryVsFinal(t *testing.T) {
	pool, ctx := setup(t, 4)
	raw, err := os.ReadFile("testdata/ledger_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Reconcile []struct {
			Status    *string `json:"status"`
			WillRetry bool    `json:"will_retry"`
			Error     string  `json:"error"`
			Calls     [][]any `json:"calls"`
			Raises    any     `json:"raises"`
		} `json:"reconcile"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	const done = "2026-01-01T00:00:00+00:00" // seed() completed_at
	checked := 0
	for i, c := range v.Reconcile {
		if c.Raises != nil {
			continue
		}
		id := fmt.Sprintf("t10-rc-%d", i)
		if c.Status != nil {
			seed(t, ctx, pool, wsA, id, *c.Status, "2026-09-01T00:00:00+00:00")
		}
		in(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			return collectledger.Reconcile(ctx, tx, id, c.WillRetry, c.Error, t0)
		})
		r := get(t, ctx, pool, wsA, id)
		var action string
		for _, call := range c.Calls {
			if call[0] == "requeue_with_error" || call[0] == "mark_failed" {
				action = call[0].(string)
				msg := call[2].(string)
				switch action {
				case "requeue_with_error":
					if r.Status != "pending" || r.Error != msg || r.CompletedAt != "" {
						t.Errorf("vector %d requeue: %+v want error %q", i, r, msg)
					}
				case "mark_failed":
					if r.Status != "failed" || r.Error != msg || r.CompletedAt != "2026-09-01T12:00:00.123456+00:00" {
						t.Errorf("vector %d final: %+v want error %q", i, r, msg)
					}
				}
			}
		}
		if action == "" && c.Status != nil && (r.Status != *c.Status || r.Error != "old err" || r.CompletedAt != done) {
			t.Errorf("vector %d (%s) must be a no-op: %+v", i, *c.Status, r)
		}
		if c.Status == nil && r != nil {
			t.Errorf("vector %d: row appeared for missing job", i)
		}
		checked++
	}
	if checked < 60 {
		t.Fatalf("only %d vectors checked", checked)
	}
}
