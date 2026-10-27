//go:build integration

package deststore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	wsA  = "ws-p10t7-a"
	wsB  = "ws-p10t7-b"
	aud  = "p10t7-aud"
	au2  = "p10t7-aud2"
	audB = "p10t7-audb"
)

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM audiences`) // cascades destinations, runs, deliveries
				return err
			})
		}
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE type = 'audience_destination_sync' AND workspace_id IN ('`+wsA+`','`+wsB+`')`)
		_, _ = pool.Exec(ctx, `DELETE FROM destination_inbound_tokens WHERE workspace_id IN ('`+wsA+`','`+wsB+`')`)
	}
	clean()
	t.Cleanup(clean)
	for _, a := range []struct{ ws, id string }{{wsA, aud}, {wsA, au2}, {wsB, audB}} {
		mustTx(t, ctx, pool, a.ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO audiences (id, workspace_id, name, filters) VALUES ($1,$2,$1,'{}'::json)`, a.id, a.ws)
			return err
		})
	}
	return pool, ctx
}

func mustTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string, fn func(pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, ws, fn); err != nil {
		t.Fatal(err)
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func mkDest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, audience, name string) deststore.Destination {
	t.Helper()
	var d deststore.Destination
	mustTx(t, ctx, pool, ws, func(tx pgx.Tx) (err error) {
		d, err = deststore.CreateDestination(ctx, tx, deststore.CreateInput{
			AudienceID: audience, Name: name, Type: "webhook", Enabled: true,
			Config: raw(`{"url":"https://hooks.example.test/x","method":"POST"}`), FieldMap: raw(`{}`),
		})
		return err
	})
	return d
}

// seedRun inserts a run row with explicit state (RLS needs the tenant tx).
func seedRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id, dest, status string, created string, failed int) {
	t.Helper()
	mustTx(t, ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO destination_runs (id, workspace_id, destination_id, status, failed, created_at)
			VALUES ($1,$2,$3,$4,$5,$6::timestamp)`, id, ws, dest, status, failed, created)
		return err
	})
}

type dlv struct {
	ID       int
	Run      string
	Dest     string
	Lead     int
	Status   string
	Attempts int
	Err      string
	Key      string
	Created  string
}

func seedDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string, d dlv) {
	t.Helper()
	if d.Key == "" {
		d.Key = fmt.Sprintf("p10t7:%s:%d", d.Dest, d.ID)
	}
	if d.Created == "" {
		d.Created = "2026-08-28 10:00:00"
	}
	var errText *string
	if d.Err != "" {
		errText = &d.Err
	}
	mustTx(t, ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO destination_deliveries (id, workspace_id, run_id, destination_id, lead_id, operation,
			idempotency_key, payload_fingerprint, status, attempts, summary, error, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,'upsert',$6,'fp',$7,$8,'',$9,$10::timestamp,$10::timestamp)`,
			d.ID, ws, d.Run, d.Dest, d.Lead, d.Key, d.Status, d.Attempts, errText, d.Created)
		return err
	})
}

func getRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id string) deststore.Run {
	t.Helper()
	var r deststore.Run
	mustTx(t, ctx, pool, ws, func(tx pgx.Tx) (err error) {
		r, err = deststore.GetRun(ctx, tx, id)
		return err
	})
	return r
}

// rlsRow runs a single-row query as workspace A (the destination tables are FORCE RLS,
// so the bare pool sees nothing).
type rlsRowT struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	q    string
	args []any
}

func rlsRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) rlsRowT {
	return rlsRowT{t, ctx, pool, q, args}
}

func (r rlsRowT) Scan(dst ...any) error {
	return tenancy.InWorkspace(r.ctx, r.pool, wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(r.ctx, r.q, r.args...).Scan(dst...)
	})
}

// statusErrs lists "status|error" of a delivery query, in order, as workspace A.
func statusErrs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string) []string {
	t.Helper()
	var out []string
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st, e string
			if err := rows.Scan(&st, &e); err != nil {
				return err
			}
			out = append(out, st+"|"+e)
		}
		return rows.Err()
	})
	return out
}

func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := rlsRow(t, ctx, pool, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- JSON shapes (Python to_api) used to compare with recorded fixtures ----

func isoNaive(t time.Time) string {
	s := t.UTC().Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

func isoPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return isoNaive(*t)
}

func strPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func jsonOf(r json.RawMessage) any {
	var v any
	if err := json.Unmarshal(r, &v); err != nil {
		panic(err)
	}
	return v
}

func apiDest(d deststore.Destination) map[string]any {
	cfg := jsonOf(d.SafeConfig())
	return map[string]any{
		"id": d.ID, "audience_id": d.AudienceID, "name": d.Name, "destination_type": d.Type, "enabled": d.Enabled,
		"config": cfg, "field_map": jsonOf(d.FieldMap), "health_status": d.HealthStatus, "last_error": strPtr(d.LastError),
		"last_success_at": isoPtr(d.LastSuccessAt), "created_at": isoNaive(d.CreatedAt), "updated_at": isoNaive(d.UpdatedAt),
	}
}

func apiRun(r deststore.Run) map[string]any {
	return map[string]any{
		"id": r.ID, "destination_id": r.DestinationID, "status": r.Status, "attempted": r.Attempted, "succeeded": r.Succeeded,
		"failed": r.Failed, "skipped": r.Skipped, "error": strPtr(r.Error), "started_at": isoPtr(r.StartedAt),
		"finished_at": isoPtr(r.FinishedAt), "created_at": isoNaive(r.CreatedAt),
	}
}

func apiDelivery(d deststore.Delivery) map[string]any {
	return map[string]any{
		"id": d.ID, "run_id": d.RunID, "destination_id": d.DestinationID, "lead_id": d.LeadID, "operation": d.Operation,
		"status": d.Status, "attempts": d.Attempts, "external_id": strPtr(d.ExternalID), "summary": strPtr(d.Summary),
		"error": strPtr(d.Error), "delivered_at": isoPtr(d.DeliveredAt), "created_at": isoNaive(d.CreatedAt),
	}
}

func apiHealth(h deststore.HealthReport) map[string]any {
	counts := map[string]any{}
	for k, v := range h.RunCounts {
		counts[k] = v
	}
	fails := []any{}
	for _, f := range h.RecentFailures {
		fails = append(fails, apiDelivery(f))
	}
	var rate any
	if h.SuccessRate != nil {
		rate = *h.SuccessRate
	}
	var latest any
	if h.LatestRun != nil {
		latest = apiRun(*h.LatestRun)
	}
	return map[string]any{
		"destination_id": h.DestinationID, "health_status": h.HealthStatus, "last_error": strPtr(h.LastError),
		"last_success_at": isoPtr(h.LastSuccessAt), "run_counts": counts,
		"delivery_counts": map[string]any{"total": h.Deliveries.Total, "succeeded": h.Deliveries.Succeeded, "failed": h.Deliveries.Failed},
		"success_rate":    rate, "consecutive_unhealthy_runs": h.ConsecutiveUnhealthyRuns, "recent_failures": fails, "latest_run": latest,
	}
}

func resp(t *testing.T, status int, v any) contract.Resp {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return contract.Resp{Status: status, Headers: map[string]string{"content-type": "application/json"}, Body: b}
}

// ---------------------------------------------------------------------------

func TestDestinationCRUDMatchesFixtures(t *testing.T) {
	pool, ctx := setup(t)
	ignore := []contract.Option{contract.IgnorePaths("$.id"), contract.IgnoreShapeOf("$.created_at", "$.updated_at")}

	for _, name := range []string{"dest_create_google_ads", "dest_create_defaults", "dest_create_disabled", "dest_create_hubspot", "dest_create_slack"} {
		fx := contract.Load(t, "destinations/"+name)
		var body struct {
			AudienceID string          `json:"audience_id"`
			Name       string          `json:"name"`
			Type       string          `json:"destination_type"`
			Enabled    *bool           `json:"enabled"`
			Config     json.RawMessage `json:"config"`
			FieldMap   json.RawMessage `json:"field_map"`
		}
		if err := json.Unmarshal(fx.Request.Body, &body); err != nil {
			t.Fatal(err)
		}
		if fx.Response.Status != 201 {
			t.Fatalf("%s: unexpected fixture status", name)
		}
		in := deststore.CreateInput{AudienceID: body.AudienceID, Name: body.Name, Type: body.Type, Enabled: true, Config: body.Config, FieldMap: body.FieldMap}
		if body.Enabled != nil {
			in.Enabled = *body.Enabled
		}
		if in.Config == nil {
			in.Config = raw(`{}`)
		}
		if in.FieldMap == nil {
			in.FieldMap = raw(`{}`)
		}
		// fixtures were recorded in workspace-a of the seed; the audience id is re-pointed here
		in.AudienceID = aud
		var d deststore.Destination
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
			d, err = deststore.CreateDestination(ctx, tx, in)
			return err
		})
		want := fx.Response
		var wb map[string]any
		_ = json.Unmarshal(want.Body, &wb)
		wb["audience_id"] = aud
		want.Body, _ = json.Marshal(wb)
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, resp(t, 201, apiDest(d)), want, ignore...) })
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error { return deststore.DeleteDestination(ctx, tx, d.ID) })
	}

	t.Run("patch", func(t *testing.T) {
		fx := contract.Load(t, "destinations/dest_patch_field_map")
		var d deststore.Destination
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
			d, err = deststore.CreateDestination(ctx, tx, deststore.CreateInput{AudienceID: aud, Name: "Renamed webhook", Type: "webhook",
				Enabled: false, Config: raw(`{"url": "https://hooks.example.test/changed", "method": "POST"}`), FieldMap: raw(`{"company": "name"}`)})
			return err
		})
		var got deststore.Destination
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
			got, err = deststore.UpdateDestination(ctx, tx, d.ID, deststore.UpdateInput{FieldMap: raw(`{"email": "e"}`)})
			return err
		})
		want := fx.Response
		var wb map[string]any
		_ = json.Unmarshal(want.Body, &wb)
		wb["audience_id"] = aud
		want.Body, _ = json.Marshal(wb)
		contract.AssertEqual(t, resp(t, 200, apiDest(got)), want, ignore...)
		if !got.UpdatedAt.After(d.UpdatedAt) && !got.UpdatedAt.Equal(d.UpdatedAt) {
			t.Errorf("updated_at moved backwards")
		}

		// config null -> {} (dest_patch_config_null_salesforce), name/enabled untouched when omitted
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
			got, err = deststore.UpdateDestination(ctx, tx, d.ID, deststore.UpdateInput{Config: raw(`{}`)})
			return err
		})
		if string(got.Config) != `{}` || got.Name != "Renamed webhook" || got.Enabled {
			t.Errorf("partial update changed other fields: %+v", got)
		}
	})

	t.Run("get_and_not_found", func(t *testing.T) {
		d := mkDest(t, ctx, pool, wsA, aud, "get-me")
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			g, err := deststore.GetDestination(ctx, tx, d.ID)
			if err != nil || g.ID != d.ID {
				t.Fatalf("get: %v %+v", err, g)
			}
			if _, err := deststore.GetDestination(ctx, tx, "nope"); !errors.Is(err, deststore.ErrNotFound) {
				t.Errorf("get unknown: %v", err)
			}
			if _, err := deststore.UpdateDestination(ctx, tx, "nope", deststore.UpdateInput{Enabled: ptr(false)}); !errors.Is(err, deststore.ErrNotFound) {
				t.Errorf("update unknown: %v", err)
			}
			if err := deststore.DeleteDestination(ctx, tx, "nope"); !errors.Is(err, deststore.ErrNotFound) {
				t.Errorf("delete unknown: %v", err)
			}
			if _, err := deststore.CreateDestination(ctx, tx, deststore.CreateInput{AudienceID: "no-such-audience", Name: "x", Type: "webhook", Config: raw(`{}`), FieldMap: raw(`{}`)}); !errors.Is(err, deststore.ErrAudienceNotFound) {
				t.Errorf("create unknown audience: %v", err)
			}
			return nil
		})
	})
}

func ptr[T any](v T) *T { return &v }

func TestDuplicateNameIsErrDuplicateOnCreateAndRename(t *testing.T) {
	pool, ctx := setup(t)
	d1 := mkDest(t, ctx, pool, wsA, aud, "dup-1")
	d2 := mkDest(t, ctx, pool, wsA, aud, "dup-2")

	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := deststore.CreateDestination(ctx, tx, deststore.CreateInput{AudienceID: aud, Name: "dup-1", Type: "webhook", Enabled: true, Config: raw(`{}`), FieldMap: raw(`{}`)})
		if !errors.Is(err, deststore.ErrDuplicate) {
			t.Errorf("create duplicate: %v", err)
		}
		// the failed statement must not poison the surrounding transaction
		if _, err := deststore.GetDestination(ctx, tx, d1.ID); err != nil {
			t.Errorf("transaction unusable after ErrDuplicate: %v", err)
		}
		_, err = deststore.UpdateDestination(ctx, tx, d2.ID, deststore.UpdateInput{Name: ptr("dup-1")})
		if !errors.Is(err, deststore.ErrDuplicate) {
			t.Errorf("rename duplicate: %v", err)
		}
		if g, err := deststore.GetDestination(ctx, tx, d2.ID); err != nil || g.Name != "dup-2" {
			t.Errorf("after failed rename: %v %+v", err, g)
		}
		// same name in another audience is fine
		if _, err := deststore.CreateDestination(ctx, tx, deststore.CreateInput{AudienceID: au2, Name: "dup-1", Type: "webhook", Enabled: true, Config: raw(`{}`), FieldMap: raw(`{}`)}); err != nil {
			t.Errorf("same name other audience: %v", err)
		}
		// renaming to its own name is not a duplicate
		if _, err := deststore.UpdateDestination(ctx, tx, d1.ID, deststore.UpdateInput{Name: ptr("dup-1")}); err != nil {
			t.Errorf("rename to own name: %v", err)
		}
		return nil
	})
}

func TestListLatestRunPerDestination(t *testing.T) {
	pool, ctx := setup(t)
	dA := mkDest(t, ctx, pool, wsA, aud, "list-a")
	dB := mkDest(t, ctx, pool, wsA, aud, "list-b")
	dC := mkDest(t, ctx, pool, wsA, au2, "list-c")
	seedRun(t, ctx, pool, wsA, "p10t7-r1", dA.ID, "completed", "2026-08-28 10:00:00", 0)
	seedRun(t, ctx, pool, wsA, "p10t7-r2", dA.ID, "failed", "2026-08-29 10:00:00", 0)
	// created_at tie: the larger id wins rank 1
	seedRun(t, ctx, pool, wsA, "p10t7-r3", dA.ID, "pending", "2026-08-29 10:00:00", 0)
	seedRun(t, ctx, pool, wsA, "p10t7-r4", dB.ID, "completed", "2026-08-27 10:00:00", 0)
	// dC has no runs; touch updated_at to order the list
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		for i, id := range []string{dA.ID, dB.ID, dC.ID} {
			if _, err := tx.Exec(ctx, `UPDATE audience_destinations SET updated_at = $2::timestamp WHERE id = $1`, id, fmt.Sprintf("2026-08-2%d 12:00:00", 1+i)); err != nil {
				return err
			}
		}
		return nil
	})

	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		all, err := deststore.ListDestinationsWithLatestRun(ctx, tx, "")
		if err != nil {
			t.Fatal(err)
		}
		var order, runs []string
		for _, x := range all {
			order = append(order, x.Name)
			if x.LatestRun != nil {
				runs = append(runs, x.Name+"="+x.LatestRun.ID)
			}
		}
		if !reflect.DeepEqual(order, []string{"list-c", "list-b", "list-a"}) {
			t.Errorf("order = %v (want updated_at DESC)", order)
		}
		if !reflect.DeepEqual(runs, []string{"list-b=p10t7-r4", "list-a=p10t7-r3"}) {
			t.Errorf("latest runs = %v", runs)
		}
		if all[0].LatestRun != nil {
			t.Errorf("destination without runs must have a nil latest run")
		}
		one, err := deststore.ListDestinationsWithLatestRun(ctx, tx, au2)
		if err != nil || len(one) != 1 || one[0].ID != dC.ID {
			t.Errorf("audience filter: %v %+v", err, one)
		}
		none, err := deststore.ListDestinationsWithLatestRun(ctx, tx, "unknown-audience")
		if err != nil || len(none) != 0 {
			t.Errorf("unknown audience: %v %v", err, none)
		}
		return nil
	})
}

func jobsFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string) []struct{ Executor, Payload, FireKey string } {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT executor, payload::text, fire_key FROM jobs
		WHERE type = 'audience_destination_sync' AND workspace_id = $1 AND status = 'pending' ORDER BY id`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct{ Executor, Payload, FireKey string }
	for rows.Next() {
		var j struct{ Executor, Payload, FireKey string }
		if err := rows.Scan(&j.Executor, &j.Payload, &j.FireKey); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func TestStartRunSingleFlightConcurrent(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "flight")
	var wg sync.WaitGroup
	var mu sync.Mutex
	var runIDs []string
	created := 0
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
				run, c, err := deststore.StartRun(ctx, tx, wsA, d.ID, "user-7", "go")
				if err != nil {
					return err
				}
				mu.Lock()
				runIDs = append(runIDs, run.ID)
				if c {
					created++
				}
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d, want exactly 1", created)
	}
	for _, id := range runIDs {
		if id != runIDs[0] {
			t.Fatalf("callers saw different runs: %v", runIDs)
		}
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM destination_runs WHERE destination_id = $1`, d.ID); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	jobs := jobsFor(t, ctx, pool, wsA)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	j := jobs[0]
	if j.Executor != "go" || j.FireKey != "destination_sync:"+runIDs[0] {
		t.Errorf("job = %+v", j)
	}
	if !reflect.DeepEqual(jsonOf(raw(j.Payload)), map[string]any{"workspace_id": wsA, "run_id": runIDs[0], "fire_key": "destination_sync:" + runIDs[0]}) {
		t.Errorf("payload = %s", j.Payload)
	}
	run := getRun(t, ctx, pool, wsA, runIDs[0])
	if run.Status != "pending" || run.RequestedBy == nil || *run.RequestedBy != "user-7" {
		t.Errorf("run = %+v", run)
	}
}

func TestStartRunAndEnqueueAtomic(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "atomic")
	// An invalid executor makes the enqueue fail after the run insert; the caller
	// then commits the transaction anyway: no run may survive.
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, created, err := deststore.StartRun(ctx, tx, wsA, d.ID, "u", "bogus"); err == nil || created {
			t.Errorf("StartRun with a bad executor: created=%v err=%v", created, err)
		}
		return nil
	})
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM destination_runs WHERE destination_id = $1`, d.ID); n != 0 {
		t.Fatalf("a run survived a failed enqueue: %d", n)
	}
	if len(jobsFor(t, ctx, pool, wsA)) != 0 {
		t.Fatal("a job exists")
	}
	// A rolled-back caller leaves neither.
	_ = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, _, err := deststore.StartRun(ctx, tx, wsA, d.ID, "u", "go"); err != nil {
			t.Fatal(err)
		}
		return errors.New("rollback")
	})
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM destination_runs WHERE destination_id = $1`, d.ID); n != 0 {
		t.Fatalf("rollback left %d runs", n)
	}
	// Disabled and unknown destinations start nothing.
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := deststore.UpdateDestination(ctx, tx, d.ID, deststore.UpdateInput{Enabled: ptr(false)}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := deststore.StartRun(ctx, tx, wsA, d.ID, "u", "go"); !errors.Is(err, deststore.ErrDisabled) {
			t.Errorf("disabled: %v", err)
		}
		if _, _, err := deststore.StartRun(ctx, tx, wsA, "nope", "u", "go"); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("unknown: %v", err)
		}
		return nil
	})
}

func TestEnqueueAudienceSyncsSkipsActive(t *testing.T) {
	pool, ctx := setup(t)
	d1 := mkDest(t, ctx, pool, wsA, aud, "enq-1")
	d2 := mkDest(t, ctx, pool, wsA, aud, "enq-2")
	d3 := mkDest(t, ctx, pool, wsA, aud, "enq-3-disabled")
	d4 := mkDest(t, ctx, pool, wsA, au2, "enq-4-other-audience")
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := deststore.UpdateDestination(ctx, tx, d3.ID, deststore.UpdateInput{Enabled: ptr(false)})
		return err
	})
	seedRun(t, ctx, pool, wsA, "p10t7-active", d2.ID, "running", "2026-08-28 10:00:00", 0)

	var asked []string
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		n, err := deststore.EnqueueAudienceSyncs(ctx, tx, wsA, aud, func(destType string) string {
			asked = append(asked, destType)
			return "go"
		})
		if err != nil || n != 1 {
			t.Fatalf("EnqueueAudienceSyncs = %d, %v (want 1: d1 only)", n, err)
		}
		// second call: d1 now has an active run too
		n, err = deststore.EnqueueAudienceSyncs(ctx, tx, wsA, aud, func(string) string { return "go" })
		if err != nil || n != 0 {
			t.Fatalf("second call = %d, %v", n, err)
		}
		return nil
	})
	if !reflect.DeepEqual(asked, []string{"webhook"}) {
		t.Errorf("executorFor calls = %v", asked)
	}
	jobs := jobsFor(t, ctx, pool, wsA)
	if len(jobs) != 1 || jobs[0].Executor != "go" {
		t.Fatalf("jobs = %+v", jobs)
	}
	var by *string
	var runID string
	if err := rlsRow(t, ctx, pool, `SELECT id, requested_by FROM destination_runs WHERE destination_id = $1`, d1.ID).Scan(&runID, &by); err != nil {
		t.Fatal(err)
	}
	if by == nil || *by != "audience_refresh" {
		t.Errorf("requested_by = %v", by)
	}
	for _, d := range []deststore.Destination{d3, d4} {
		if n := countRows(t, ctx, pool, `SELECT count(*) FROM destination_runs WHERE destination_id = $1`, d.ID); n != 0 {
			t.Errorf("%s got a run", d.Name)
		}
	}
	if err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := deststore.EnqueueAudienceSyncs(ctx, tx, wsA, au2, func(string) string { return "bogus" })
		if err == nil {
			t.Error("an invalid executor must fail")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM destination_runs WHERE destination_id = $1`, d4.ID); n != 0 {
		t.Error("a failed enqueue left a run behind")
	}
}

func TestFinishCancelledRecomputesCounts(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "cancel-me")
	seedRun(t, ctx, pool, wsA, "p10t7-c1", d.ID, "cancelling", "2026-08-28 10:00:00", 0)
	for i, st := range []string{"success", "success", "failed", "skipped", "pending", "in_flight"} {
		attempts := 1
		if st == "pending" || st == "skipped" {
			attempts = 0
		}
		seedDelivery(t, ctx, pool, wsA, dlv{ID: 7100 + i, Run: "p10t7-c1", Dest: d.ID, Lead: 1 + i, Status: st, Attempts: attempts})
	}
	var out deststore.Run
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		run, err := deststore.GetRun(ctx, tx, "p10t7-c1")
		if err != nil {
			return err
		}
		dest, err := deststore.GetDestination(ctx, tx, d.ID)
		if err != nil {
			return err
		}
		out, err = deststore.FinishCancelled(ctx, tx, run, &dest)
		return err
	})
	if out.Status != "cancelled" || out.Error == nil || *out.Error != "Cancellation completed" || out.FinishedAt == nil {
		t.Fatalf("run = %+v", out)
	}
	// attempted counts attempts>0 (the in-flight one included), cancelled ones keep their attempts
	if out.Attempted != 4 || out.Succeeded != 2 || out.Failed != 1 || out.Skipped != 1 {
		t.Errorf("counts = %d/%d/%d/%d", out.Attempted, out.Succeeded, out.Failed, out.Skipped)
	}
	got := statusErrs(t, ctx, pool, `SELECT status, coalesce(error,'') FROM destination_deliveries WHERE run_id = 'p10t7-c1' ORDER BY id`)
	want := []string{"success|", "success|", "failed|", "skipped|", "cancelled|Run cancelled before delivery", "cancelled|Run cancelled before delivery"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deliveries = %v", got)
	}
	var lastSuccess *time.Time
	if err := rlsRow(t, ctx, pool, `SELECT last_success_at FROM audience_destinations WHERE id = $1`, d.ID).Scan(&lastSuccess); err != nil {
		t.Fatal(err)
	}
	if lastSuccess == nil || !lastSuccess.Equal(*out.FinishedAt) {
		t.Errorf("last_success_at = %v, finished_at = %v", lastSuccess, out.FinishedAt)
	}

	// nothing succeeded -> last_success_at untouched; nil destination tolerated
	seedRun(t, ctx, pool, wsA, "p10t7-c2", d.ID, "cancelling", "2026-08-28 11:00:00", 0)
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		run, _ := deststore.GetRun(ctx, tx, "p10t7-c2")
		r, err := deststore.FinishCancelled(ctx, tx, run, nil)
		if err != nil || r.Status != "cancelled" || r.Attempted != 0 {
			t.Errorf("nil destination: %v %+v", err, r)
		}
		return nil
	})
}

func TestCancelAndRetryRuns(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "cancel-retry")
	seedRun(t, ctx, pool, wsA, "p10t7-k-pending", d.ID, "pending", "2026-08-28 10:00:00", 0)
	seedRun(t, ctx, pool, wsA, "p10t7-k-running", d.ID, "running", "2026-08-28 10:01:00", 0)
	seedRun(t, ctx, pool, wsA, "p10t7-k-done", d.ID, "completed", "2026-08-28 10:02:00", 0)
	seedRun(t, ctx, pool, wsA, "p10t7-k-failed", d.ID, "failed", "2026-08-28 10:03:00", 3)
	seedDelivery(t, ctx, pool, wsA, dlv{ID: 7200, Run: "p10t7-k-pending", Dest: d.ID, Lead: 1, Status: "pending"})

	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		r, prev, err := deststore.CancelRun(ctx, tx, "p10t7-k-pending")
		if err != nil || prev != "pending" || r.Status != "cancelled" {
			t.Errorf("cancel pending: %v %q %+v", err, prev, r)
		}
		r, prev, err = deststore.CancelRun(ctx, tx, "p10t7-k-running")
		if err != nil || prev != "running" || r.Status != "cancelling" || r.Error == nil || *r.Error != "Cancellation requested" {
			t.Errorf("cancel running: %v %q %+v", err, prev, r)
		}
		r, prev, err = deststore.CancelRun(ctx, tx, "p10t7-k-running")
		if err != nil || prev != "cancelling" || r.Status != "cancelling" {
			t.Errorf("cancel again: %v %q %+v", err, prev, r)
		}
		if _, _, err := deststore.CancelRun(ctx, tx, "p10t7-k-done"); !errors.Is(err, deststore.ErrNotCancellable) {
			t.Errorf("cancel completed: %v", err)
		}
		if _, _, err := deststore.CancelRun(ctx, tx, "nope"); !errors.Is(err, deststore.ErrRunNotFound) {
			t.Errorf("cancel unknown: %v", err)
		}
		// retry
		if _, err := deststore.RetryRun(ctx, tx, "p10t7-k-done", func(string) string { return "go" }); !errors.Is(err, deststore.ErrNotRetryable) {
			t.Errorf("retry completed: %v", err)
		}
		if _, err := deststore.RetryRun(ctx, tx, "nope", func(string) string { return "go" }); !errors.Is(err, deststore.ErrRunNotFound) {
			t.Errorf("retry unknown: %v", err)
		}
		r, err = deststore.RetryRun(ctx, tx, "p10t7-k-failed", func(ty string) string {
			if ty != "webhook" {
				t.Errorf("executorFor(%q)", ty)
			}
			return "go"
		})
		if err != nil || r.Status != "pending" || r.Error != nil || r.FinishedAt != nil || r.Failed != 3 {
			t.Errorf("retry failed: %v %+v", err, r)
		}
		return nil
	})
	jobs := jobsFor(t, ctx, pool, wsA)
	if len(jobs) != 1 || jobs[0].FireKey != "destination_sync:p10t7-k-failed" || jobs[0].Executor != "go" {
		t.Errorf("jobs = %+v", jobs)
	}
	var st string
	if err := rlsRow(t, ctx, pool, `SELECT status FROM destination_deliveries WHERE id = 7200`).Scan(&st); err != nil || st != "cancelled" {
		t.Errorf("pending run's delivery = %q %v", st, err)
	}
	// a disabled destination cannot be retried
	seedRun(t, ctx, pool, wsA, "p10t7-k-failed2", d.ID, "failed", "2026-08-28 10:04:00", 1)
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := deststore.UpdateDestination(ctx, tx, d.ID, deststore.UpdateInput{Enabled: ptr(false)}); err != nil {
			return err
		}
		if _, err := deststore.RetryRun(ctx, tx, "p10t7-k-failed2", func(string) string { return "go" }); !errors.Is(err, deststore.ErrDisabled) {
			t.Errorf("retry disabled: %v", err)
		}
		return nil
	})
	if run := getRun(t, ctx, pool, wsA, "p10t7-k-failed2"); run.Status != "failed" {
		t.Errorf("a refused retry changed the run: %+v", run)
	}
}

func TestRunsCursorPagination(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "pages")
	// five runs; two share a created_at (tie broken by id DESC)
	for i, c := range []string{"2026-08-28 10:00:00", "2026-08-28 10:00:01", "2026-08-28 10:00:02", "2026-08-28 10:00:02", "2026-08-28 10:00:03.123456"} {
		seedRun(t, ctx, pool, wsA, fmt.Sprintf("p10t7-p%d", i), d.ID, "completed", c, 0)
	}
	ids := func(p deststore.RunsPage) []string {
		var out []string
		for _, r := range p.Runs {
			out = append(out, r.ID)
		}
		return out
	}
	var all []string
	var cur *deststore.RunsCursor
	for page := 0; page < 5; page++ {
		var p deststore.RunsPage
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
			p, err = deststore.ListRuns(ctx, tx, d.ID, deststore.RunsQuery{Limit: 2, Cursor: cur})
			return err
		})
		all = append(all, ids(p)...)
		if page < 2 && !p.HasMore {
			t.Fatalf("page %d: has_more false", page)
		}
		if !p.HasMore {
			if page != 2 || len(p.Runs) != 1 {
				t.Fatalf("last page = %d runs at page %d", len(p.Runs), page)
			}
			break
		}
		last := p.Runs[len(p.Runs)-1]
		cur = &deststore.RunsCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	want := []string{"p10t7-p4", "p10t7-p3", "p10t7-p2", "p10t7-p1", "p10t7-p0"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("walk = %v, want %v", all, want)
	}
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := deststore.ListRuns(ctx, tx, "nope", deststore.RunsQuery{Limit: 2}); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("unknown destination: %v", err)
		}
		return nil
	})
}

func TestRunsOffsetWithoutCursor(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "offsets")
	for i := 0; i < 5; i++ {
		seedRun(t, ctx, pool, wsA, fmt.Sprintf("p10t7-o%d", i), d.ID, "completed", fmt.Sprintf("2026-08-28 10:00:0%d", i), 0)
	}
	list := func(q deststore.RunsQuery) (out []string, more bool) {
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			p, err := deststore.ListRuns(ctx, tx, d.ID, q)
			for _, r := range p.Runs {
				out = append(out, r.ID)
			}
			more = p.HasMore
			return err
		})
		return
	}
	if got, more := list(deststore.RunsQuery{Limit: 2, Offset: 1}); !reflect.DeepEqual(got, []string{"p10t7-o3", "p10t7-o2"}) || !more {
		t.Errorf("offset 1 = %v more=%v", got, more)
	}
	if got, more := list(deststore.RunsQuery{Limit: 2, Offset: 4}); !reflect.DeepEqual(got, []string{"p10t7-o0"}) || more {
		t.Errorf("offset 4 = %v more=%v", got, more)
	}
	if got, more := list(deststore.RunsQuery{Limit: 2, Offset: 100}); len(got) != 0 || more {
		t.Errorf("offset past end = %v more=%v", got, more)
	}
	// a cursor wins over the offset
	if got, _ := list(deststore.RunsQuery{Limit: 2, Offset: 3, Cursor: &deststore.RunsCursor{CreatedAt: time.Date(2026, 8, 28, 10, 0, 3, 0, time.UTC), ID: "p10t7-o3"}}); !reflect.DeepEqual(got, []string{"p10t7-o2", "p10t7-o1"}) {
		t.Errorf("cursor+offset = %v", got)
	}
}

func TestHealthMatchesFixtures(t *testing.T) {
	pool, ctx := setup(t)
	fx := contract.Load(t, "destinations/health_after_changes")
	var want map[string]any
	if err := json.Unmarshal(fx.Response.Body, &want); err != nil {
		t.Fatal(err)
	}
	// seed exactly the state the recorded response was taken from
	var d deststore.Destination
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		d, err = deststore.CreateDestination(ctx, tx, deststore.CreateInput{AudienceID: aud, Name: "fx-health", Type: "webhook", Enabled: true, Config: raw(`{}`), FieldMap: raw(`{}`)})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE audience_destinations SET health_status='healthy', last_success_at='2026-08-28 11:00:00' WHERE id=$1`, d.ID)
		return err
	})
	runs := []struct {
		id, status, created string
	}{
		{"run-act-w1", "completed", "2026-08-28 05:00:00"}, {"run-act-w2", "completed", "2026-08-28 06:00:00"},
		{"run-act-w3", "cancelled", "2026-08-28 07:00:00"}, {"run-act-w4", "completed", "2026-08-28 08:00:00"},
		{"run-act-w5", "failed", "2026-08-28 09:00:00"}, {"run-act-w6", "pending", "2026-08-28 10:00:00"},
	}
	for _, r := range runs {
		seedRun(t, ctx, pool, wsA, r.id, d.ID, r.status, r.created, 0)
	}
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE destination_runs SET attempted=3, failed=3, started_at='2026-08-28 10:00:00' WHERE id='run-act-w6'`)
		return err
	})
	for _, x := range []dlv{
		{ID: 1, Run: "run-act-w1", Lead: 1, Status: "success", Attempts: 1}, {ID: 2, Run: "run-act-w2", Lead: 2, Status: "success", Attempts: 1},
		{ID: 3, Run: "run-act-w6", Lead: 3, Status: "pending"}, {ID: 4, Run: "run-act-w6", Lead: 6, Status: "pending"},
		{ID: 7, Run: "run-act-w5", Lead: 10, Status: "failed", Attempts: 2, Err: "webhook HTTP 502: bad gateway", Created: "2026-08-28 09:00:25"},
		{ID: 8, Run: "run-act-w6", Lead: 4, Status: "failed", Attempts: 1, Err: "webhook HTTP 500: boom", Created: "2026-08-28 10:00:05"},
		{ID: 9, Run: "run-act-w6", Lead: 5, Status: "failed", Attempts: 1, Err: "webhook HTTP 500: boom", Created: "2026-08-28 10:00:06"},
	} {
		x.Dest = d.ID
		seedDelivery(t, ctx, pool, wsA, x)
	}
	var h deststore.HealthReport
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		h, err = deststore.Health(ctx, tx, d.ID)
		return err
	})
	want["destination_id"] = d.ID
	want["latest_run"].(map[string]any)["destination_id"] = d.ID
	for _, f := range want["recent_failures"].([]any) {
		f.(map[string]any)["destination_id"] = d.ID
	}
	wantBody, _ := json.Marshal(want)
	contract.AssertEqual(t, resp(t, 200, apiHealth(h)), contract.Resp{Status: 200, Headers: map[string]string{"content-type": "application/json"}, Body: wantBody})

	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := deststore.Health(ctx, tx, "nope"); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("unknown destination: %v", err)
		}
		return nil
	})

	// a destination with no history: null rate, null latest run, empty maps
	empty := mkDest(t, ctx, pool, wsA, aud, "fx-empty")
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		h, err = deststore.Health(ctx, tx, empty.ID)
		return err
	})
	if h.SuccessRate != nil || h.LatestRun != nil || len(h.RunCounts) != 0 || h.Deliveries.Total != 0 || h.ConsecutiveUnhealthyRuns != 0 || h.RecentFailures == nil || len(h.RecentFailures) != 0 {
		t.Errorf("empty health = %+v", h)
	}
}

func TestHealthConsecutiveWindow(t *testing.T) {
	// only the newest 20 runs are inspected; completed_with_errors counts as unhealthy
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "unhealthy")
	bad := "completed_with_errors"
	for i := 0; i < 25; i++ {
		st := bad
		if i%2 == 1 {
			st = "failed"
		}
		if i == 24 { // oldest is healthy, beyond the window anyway
			st = "completed"
		}
		seedRun(t, ctx, pool, wsA, fmt.Sprintf("p10t7-h%02d", i), d.ID, st, fmt.Sprintf("2026-08-28 10:%02d:00", 59-i), 0)
	}
	var h deststore.HealthReport
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		h, err = deststore.Health(ctx, tx, d.ID)
		return err
	})
	if h.ConsecutiveUnhealthyRuns != 20 {
		t.Errorf("consecutive = %d, want 20 (window)", h.ConsecutiveUnhealthyRuns)
	}
	if h.RunCounts["failed"] == 0 || h.RunCounts["completed"] != 1 {
		t.Errorf("run_counts = %v", h.RunCounts)
	}
}

func TestDeliveriesFilterAndOrder(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "deliv")
	seedRun(t, ctx, pool, wsA, "p10t7-dv", d.ID, "running", "2026-08-28 10:00:00", 0)
	seedRun(t, ctx, pool, wsA, "p10t7-dv-other", d.ID, "running", "2026-08-28 10:01:00", 0)
	for i, st := range []string{"success", "failed", "pending", "failed", "in_flight"} {
		seedDelivery(t, ctx, pool, wsA, dlv{ID: 7300 + 10 - i*2, Run: "p10t7-dv", Dest: d.ID, Lead: i, Status: st}) // ids descend with insertion order
	}
	seedDelivery(t, ctx, pool, wsA, dlv{ID: 7399, Run: "p10t7-dv-other", Dest: d.ID, Lead: 99, Status: "failed"})
	idsOf := func(status string, after *int, limit int) []int {
		var out []int
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			ds, err := deststore.ListDeliveries(ctx, tx, "p10t7-dv", status, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range ds {
				out = append(out, x.ID)
			}
			return nil
		})
		return out
	}
	if got := idsOf("", nil, 100); !reflect.DeepEqual(got, []int{7302, 7304, 7306, 7308, 7310}) {
		t.Errorf("all = %v (id ASC, this run only)", got)
	}
	if got := idsOf("failed", nil, 100); !reflect.DeepEqual(got, []int{7304, 7308}) {
		t.Errorf("failed = %v", got)
	}
	if got := idsOf("", ptr(7304), 2); !reflect.DeepEqual(got, []int{7306, 7308}) {
		t.Errorf("after_id+limit = %v", got)
	}
	if got := idsOf("", ptr(-5), 1); !reflect.DeepEqual(got, []int{7302}) {
		t.Errorf("negative after_id = %v", got)
	}
	if got := idsOf("", ptr(99999), 10); len(got) != 0 {
		t.Errorf("past end = %v", got)
	}
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := deststore.ListDeliveries(ctx, tx, "nope", "", nil, 10); !errors.Is(err, deststore.ErrRunNotFound) {
			t.Errorf("unknown run: %v", err)
		}
		return nil
	})
}

func TestUpsertAndSettleDelivery(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "upsert")
	seedRun(t, ctx, pool, wsA, "p10t7-u1", d.ID, "running", "2026-08-28 10:00:00", 0)
	seedRun(t, ctx, pool, wsA, "p10t7-u2", d.ID, "running", "2026-08-28 11:00:00", 0)
	in := deststore.UpsertDeliveryInput{RunID: "p10t7-u1", DestinationID: d.ID, LeadID: 5, Operation: "upsert",
		IdempotencyKey: "dest:x:lead:5:abc", Fingerprint: "abc", Payload: raw(`{"email":"h"}`), Status: "in_flight", Attempts: 1}
	var first, second deststore.Delivery
	var out1, out2, out3 deststore.UpsertOutcome
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		first, out1, err = deststore.UpsertDelivery(ctx, tx, in)
		if err != nil {
			return err
		}
		// settle as failed with an overlong error: truncated to 1000 runes
		_, err = deststore.SettleDelivery(ctx, tx, first.ID, deststore.SettleInput{Status: "failed", Summary: ptr(""), Error: strings.Repeat("é", 1500)})
		return err
	})
	if out1 != deststore.Inserted || first.Attempts != 1 || first.Status != "in_flight" {
		t.Fatalf("first = %+v %v", first, out1)
	}
	in.RunID = "p10t7-u2"
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		second, out2, err = deststore.UpsertDelivery(ctx, tx, in) // a later manual run revives the failed effect
		return err
	})
	if out2 != deststore.Revived || second.ID != first.ID || second.RunID != "p10t7-u2" || second.Attempts != 2 || second.Error != nil || second.Status != "in_flight" {
		t.Fatalf("second = %+v %v", second, out2)
	}
	var settled deststore.Delivery
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		settled, err = deststore.SettleDelivery(ctx, tx, first.ID, deststore.SettleInput{Status: "success", Summary: ptr("sent"), ExternalID: ptr("ext-1"), Delivered: true})
		return err
	})
	if settled.Status != "success" || settled.DeliveredAt == nil || settled.ExternalID == nil || *settled.ExternalID != "ext-1" || settled.Error != nil {
		t.Fatalf("settled = %+v", settled)
	}
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		third, o, err := deststore.UpsertDelivery(ctx, tx, in) // success rows are never touched
		out3 = o
		if third.ID != first.ID || third.Status != "success" {
			t.Errorf("third = %+v", third)
		}
		return err
	})
	if out3 != deststore.AlreadySucceeded {
		t.Errorf("outcome = %v", out3)
	}
	// the overlong error was cut to 1000 runes before the revive cleared it
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		x, err := deststore.SettleDelivery(ctx, tx, first.ID, deststore.SettleInput{Status: "failed", Error: strings.Repeat("é", 1500)})
		if err != nil || x.Error == nil || len([]rune(*x.Error)) != 1000 {
			t.Errorf("error truncation: %v %v", err, x.Error)
		}
		if _, err := deststore.SettleDelivery(ctx, tx, 987654321, deststore.SettleInput{Status: "failed"}); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("settle unknown: %v", err)
		}
		return nil
	})
}

func TestReconcileFailureRetryResetsInFlight(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "reconcile-retry")
	seedRun(t, ctx, pool, wsA, "p10t7-f1", d.ID, "running", "2026-08-28 10:00:00", 0)
	for i, st := range []string{"success", "in_flight", "pending", "failed"} {
		seedDelivery(t, ctx, pool, wsA, dlv{ID: 7400 + i, Run: "p10t7-f1", Dest: d.ID, Lead: i, Status: st, Attempts: 1})
	}
	reconcile(t, ctx, pool, "p10t7-f1", true, "boom")
	run := getRun(t, ctx, pool, wsA, "p10t7-f1")
	if run.Status != "pending" || run.Error == nil || *run.Error != "Queue retry scheduled: boom" || run.FinishedAt != nil {
		t.Fatalf("run = %+v", run)
	}
	got := statusErrs(t, ctx, pool, `SELECT status, coalesce(error,'') FROM destination_deliveries WHERE run_id='p10t7-f1' ORDER BY id`)
	if !reflect.DeepEqual(got, []string{"success|", "pending|Queue retry scheduled: boom", "pending|", "failed|"}) {
		t.Errorf("deliveries = %v", got)
	}
	var health string
	_ = rlsRow(t, ctx, pool, `SELECT health_status FROM audience_destinations WHERE id=$1`, d.ID).Scan(&health)
	if health != "unverified" {
		t.Errorf("a retry must not degrade the destination: %s", health)
	}
}

func reconcile(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID string, willRetry bool, msg string) {
	t.Helper()
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		run, err := deststore.GetRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		return deststore.ReconcileFailure(ctx, tx, run, willRetry, msg)
	})
}

func TestReconcileFailureTerminalFailsPending(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "reconcile-final")
	seedRun(t, ctx, pool, wsA, "p10t7-g1", d.ID, "running", "2026-08-28 10:00:00", 0)
	for i, st := range []string{"success", "in_flight", "pending", "failed", "skipped"} {
		att := 1
		if st == "pending" || st == "skipped" {
			att = 0
		}
		seedDelivery(t, ctx, pool, wsA, dlv{ID: 7500 + i, Run: "p10t7-g1", Dest: d.ID, Lead: i, Status: st, Attempts: att})
	}
	reconcile(t, ctx, pool, "p10t7-g1", false, strings.Repeat("x", 1500))
	run := getRun(t, ctx, pool, wsA, "p10t7-g1")
	if run.Status != "failed" || run.Error == nil || *run.Error != "Final failure: "+strings.Repeat("x", 1000) || run.FinishedAt == nil {
		t.Fatalf("run = %+v", run)
	}
	// success, in_flight->failed, pending->failed, failed, skipped
	if run.Attempted != 3 || run.Succeeded != 1 || run.Failed != 3 || run.Skipped != 1 {
		t.Errorf("counts = %d/%d/%d/%d", run.Attempted, run.Succeeded, run.Failed, run.Skipped)
	}
	var health string
	var lastErr *string
	_ = rlsRow(t, ctx, pool, `SELECT health_status, last_error FROM audience_destinations WHERE id=$1`, d.ID).Scan(&health, &lastErr)
	if health != "degraded" || lastErr == nil || *lastErr != *run.Error {
		t.Errorf("destination = %s %v", health, lastErr)
	}
	// empty message falls back to Python's default
	seedRun(t, ctx, pool, wsA, "p10t7-g2", d.ID, "running", "2026-08-28 11:00:00", 0)
	reconcile(t, ctx, pool, "p10t7-g2", false, "")
	if r := getRun(t, ctx, pool, wsA, "p10t7-g2"); r.Error == nil || *r.Error != "Final failure: destination sync attempt failed" {
		t.Errorf("default message: %+v", r.Error)
	}
	// terminal runs and unknown runs are left alone
	for _, st := range []string{"completed", "cancelled"} {
		id := "p10t7-g-" + st
		seedRun(t, ctx, pool, wsA, id, d.ID, st, "2026-08-28 12:00:00", 0)
		reconcile(t, ctx, pool, id, false, "late")
		if r := getRun(t, ctx, pool, wsA, id); r.Status != st || r.Error != nil {
			t.Errorf("%s run touched: %+v", st, r)
		}
	}
	seedRun(t, ctx, pool, wsA, "p10t7-g-cwe", d.ID, "completed_with_errors", "2026-08-28 12:30:00", 1)
	reconcile(t, ctx, pool, "p10t7-g-cwe", false, "late")
	if r := getRun(t, ctx, pool, wsA, "p10t7-g-cwe"); r.Status != "completed_with_errors" {
		t.Errorf("completed_with_errors touched: %+v", r)
	}
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if err := deststore.ReconcileFailure(ctx, tx, deststore.Run{ID: "gone", WorkspaceID: wsA}, false, "x"); err != nil {
			t.Errorf("unknown run: %v", err)
		}
		return nil
	})
}

func TestReconcileCancellingFinishesCancelled(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "reconcile-cancel")
	seedRun(t, ctx, pool, wsA, "p10t7-h1", d.ID, "cancelling", "2026-08-28 10:00:00", 0)
	seedDelivery(t, ctx, pool, wsA, dlv{ID: 7600, Run: "p10t7-h1", Dest: d.ID, Lead: 1, Status: "success", Attempts: 1})
	seedDelivery(t, ctx, pool, wsA, dlv{ID: 7601, Run: "p10t7-h1", Dest: d.ID, Lead: 2, Status: "in_flight", Attempts: 1})
	reconcile(t, ctx, pool, "p10t7-h1", true, "killed")
	run := getRun(t, ctx, pool, wsA, "p10t7-h1")
	if run.Status != "cancelled" || run.Error == nil || *run.Error != "Cancellation completed" || run.Succeeded != 1 || run.Attempted != 2 {
		t.Fatalf("run = %+v", run)
	}
	var st string
	_ = rlsRow(t, ctx, pool, `SELECT status FROM destination_deliveries WHERE id = 7601`).Scan(&st)
	if st != "cancelled" {
		t.Errorf("in-flight delivery = %s", st)
	}
	var ls *time.Time
	_ = rlsRow(t, ctx, pool, `SELECT last_success_at FROM audience_destinations WHERE id=$1`, d.ID).Scan(&ls)
	if ls == nil {
		t.Error("last_success_at not set although a delivery succeeded")
	}
}

func TestDeleteRevokesTokens(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "with-tokens")
	other := mkDest(t, ctx, pool, wsA, aud, "other-dest")
	seedRun(t, ctx, pool, wsA, "p10t7-t1", d.ID, "completed", "2026-08-28 10:00:00", 0)
	seedDelivery(t, ctx, pool, wsA, dlv{ID: 7700, Run: "p10t7-t1", Dest: d.ID, Lead: 1, Status: "success", Attempts: 1})
	for i, x := range []struct{ id, ws, dest string }{{"p10t7-tok1", wsA, d.ID}, {"p10t7-tok2", wsA, d.ID}, {"p10t7-tok3", wsA, other.ID}, {"p10t7-tok4", wsB, d.ID}} {
		if _, err := pool.Exec(ctx, `INSERT INTO destination_inbound_tokens (id, workspace_id, destination_id, token_hash, prefix) VALUES ($1,$2,$3,$4,'pfx')`,
			x.id, x.ws, x.dest, fmt.Sprintf("p10t7hash%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error { return deststore.DeleteDestination(ctx, tx, d.ID) })
	rows, err := pool.Query(ctx, `SELECT id, revoked_at IS NOT NULL FROM destination_inbound_tokens WHERE id LIKE 'p10t7-tok%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var id string
		var rev bool
		_ = rows.Scan(&id, &rev)
		got[id] = rev
	}
	// the foreign-workspace token with the same destination id is untouched; the other destination's too
	if !got["p10t7-tok1"] || !got["p10t7-tok2"] || got["p10t7-tok3"] || got["p10t7-tok4"] {
		t.Errorf("revocations = %v", got)
	}
	if n := countRows(t, ctx, pool, `SELECT (SELECT count(*) FROM destination_runs WHERE destination_id=$1) + (SELECT count(*) FROM destination_deliveries WHERE destination_id=$1)`, d.ID); n != 0 {
		t.Errorf("runs/deliveries survived the delete: %d", n)
	}
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := deststore.GetDestination(ctx, tx, d.ID); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("deleted destination still readable: %v", err)
		}
		return nil
	})
}

func TestTenantIsolation(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "tenant")
	seedRun(t, ctx, pool, wsA, "p10t7-x1", d.ID, "failed", "2026-08-28 10:00:00", 1)
	seedDelivery(t, ctx, pool, wsA, dlv{ID: 7800, Run: "p10t7-x1", Dest: d.ID, Lead: 1, Status: "failed", Attempts: 1})
	// a same-named destination in B shares the audience id but is its own row
	b := mkDest(t, ctx, pool, wsB, audB, "tenant")

	mustTx(t, ctx, pool, wsB, func(tx pgx.Tx) error {
		if _, err := deststore.GetDestination(ctx, tx, d.ID); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("get foreign: %v", err)
		}
		if _, err := deststore.UpdateDestination(ctx, tx, d.ID, deststore.UpdateInput{Enabled: ptr(false)}); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("update foreign: %v", err)
		}
		if _, err := deststore.Health(ctx, tx, d.ID); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("health foreign: %v", err)
		}
		if _, err := deststore.ListRuns(ctx, tx, d.ID, deststore.RunsQuery{Limit: 10}); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("runs foreign: %v", err)
		}
		if _, err := deststore.ListDeliveries(ctx, tx, "p10t7-x1", "", nil, 10); !errors.Is(err, deststore.ErrRunNotFound) {
			t.Errorf("deliveries foreign: %v", err)
		}
		if _, _, err := deststore.CancelRun(ctx, tx, "p10t7-x1"); !errors.Is(err, deststore.ErrRunNotFound) {
			t.Errorf("cancel foreign: %v", err)
		}
		if _, err := deststore.RetryRun(ctx, tx, "p10t7-x1", func(string) string { return "go" }); !errors.Is(err, deststore.ErrRunNotFound) {
			t.Errorf("retry foreign: %v", err)
		}
		if _, _, err := deststore.StartRun(ctx, tx, wsB, d.ID, "u", "go"); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("start foreign: %v", err)
		}
		if err := deststore.DeleteDestination(ctx, tx, d.ID); !errors.Is(err, deststore.ErrNotFound) {
			t.Errorf("delete foreign: %v", err)
		}
		if err := deststore.ReconcileFailure(ctx, tx, deststore.Run{ID: "p10t7-x1", WorkspaceID: wsA}, false, "x"); err == nil {
			t.Error("ReconcileFailure accepted a run of a workspace that is not the tenant")
		}
		if err := deststore.ReconcileFailure(ctx, tx, deststore.Run{ID: "p10t7-x1", WorkspaceID: wsB}, false, "x"); err != nil {
			t.Errorf("reconcile of an id that is not B's: %v", err)
		}
		all, err := deststore.ListDestinationsWithLatestRun(ctx, tx, "")
		if err != nil || len(all) != 1 || all[0].ID != b.ID || all[0].LatestRun != nil {
			t.Errorf("list in B: %v %+v", err, all)
		}
		// a workspace argument that is not the transaction tenant is refused
		if _, _, err := deststore.StartRun(ctx, tx, wsA, b.ID, "u", "go"); err == nil {
			t.Error("StartRun accepted a workspace that is not the tenant")
		}
		if _, err := deststore.EnqueueAudienceSyncs(ctx, tx, wsA, aud, func(string) string { return "go" }); err == nil {
			t.Error("EnqueueAudienceSyncs accepted a workspace that is not the tenant")
		}
		return nil
	})
	if run := getRun(t, ctx, pool, wsA, "p10t7-x1"); run.Status != "failed" || run.Error != nil {
		t.Errorf("A's run was touched from B: %+v", run)
	}
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := deststore.GetDestination(ctx, tx, d.ID); err != nil {
			t.Errorf("A lost its destination: %v", err)
		}
		return nil
	})
}

// Plan 10 final correctness L3: FinishCancelled takes the run lock before it touches the deliveries (the order
// ReconcileFailure uses), so a stale worker cannot hold deliveries while waiting for the run.
func TestFinishCancelledLocksRunBeforeDeliveries(t *testing.T) {
	pool, ctx := setup(t)
	d := mkDest(t, ctx, pool, wsA, aud, "lock-order")
	seedRun(t, ctx, pool, wsA, "p10fin-l3", d.ID, "cancelling", "2026-08-28 10:00:00", 0)
	seedDelivery(t, ctx, pool, wsA, dlv{ID: 7300, Run: "p10fin-l3", Dest: d.ID, Lead: 1, Status: "pending"})

	held, release := make(chan struct{}), make(chan struct{})
	holderDone := make(chan error, 1)
	go func() { // simulates the hook: owns a delivery row
		holderDone <- tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE destination_deliveries SET attempts = attempts WHERE id = 7300`); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	finishDone := make(chan error, 1)
	go func() {
		finishDone <- tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
			run, err := deststore.GetRun(ctx, tx, "p10fin-l3")
			if err != nil {
				return err
			}
			_, err = deststore.FinishCancelled(ctx, tx, run, nil)
			return err
		})
	}()
	time.Sleep(700 * time.Millisecond) // FinishCancelled is now waiting for the delivery row
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1 FROM destination_runs WHERE id = 'p10fin-l3' FOR UPDATE NOWAIT`)
		return err
	})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Errorf("run row was not locked while FinishCancelled waited on deliveries: %v", err)
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-finishDone; err != nil {
		t.Fatal(err)
	}
}
