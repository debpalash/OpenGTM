//go:build integration

package automationhook_test

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const emWS = "ws-ah-emit"

func emSetup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := setup(t)
	clean := func() {
		for _, w := range []string{emWS, "ws-ah-emit2"} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				for _, q := range []string{`DELETE FROM triggers`, `DELETE FROM workbook_rows`, `DELETE FROM workbooks`} {
					if _, err := tx.Exec(ctx, q); err != nil {
						return err
					}
				}
				return nil
			})
		}
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE type='trigger_eval'`)
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func emTrigger(t *testing.T, pool *pgxpool.Pool, ctx context.Context, w, id, typ, cfg, scope string, enabled bool) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, actions, scope_workbook_ids)
			VALUES ($1,$2,'n',$3,$4,$5::json,'[]'::json,$6::json)`, id, w, enabled, typ, cfg, scope)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func emRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context, w string, leads map[string][]int) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
		for wb, ls := range leads {
			if _, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1,'n',$2) ON CONFLICT DO NOTHING`, wb, w); err != nil {
				return err
			}
			for _, l := range ls {
				if _, err := tx.Exec(ctx, `INSERT INTO workbook_rows (workbook_id, workspace_id, lead_id, data) VALUES ($1,$2,$3,'{}'::json)`, wb, w, l); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type emitter = automationhook.Emitter

func inTx(t *testing.T, pool *pgxpool.Pool, ctx context.Context, w string, f func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, w, f); err != nil {
		t.Fatal(err)
	}
}

type pl struct {
	TriggerID  string              `json:"trigger_id"`
	Workspace  string              `json:"workspace_id"`
	FireSource string              `json:"fire_source"`
	FireKey    string              `json:"fire_key"`
	DryRun     bool                `json:"dry_run"`
	RawTargets []map[string]string `json:"targets"`
}

func payloads(t *testing.T, pool *pgxpool.Pool, ctx context.Context) []pl {
	t.Helper()
	var out []pl
	for _, j := range jobs(t, pool, ctx) {
		var p pl
		if err := json.Unmarshal([]byte(j.Payload), &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func newE(enabled bool) emitter {
	return automationhook.NewWithExecutor(enabled, nil, slog.Default())
}

func TestRowsChangedFireKeyVectors(t *testing.T) {
	pool, ctx := emSetup(t)
	emTrigger(t, pool, ctx, emWS, "c1", "on_row_changed", `{}`, `[]`, true)
	var n int
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) (err error) {
		n, err = newE(true).RowsChanged(ctx, tx, emWS, "wb-1", []automationhook.Change{
			{RowID: 5, Field: "email", Old: nil, New: "a@b.c", CellVersion: "1700000000000"},
			{RowID: 6, Field: "n", Old: 1.5, New: true, CellVersion: ""},
		})
		return
	})
	if n != 2 {
		t.Fatalf("n=%d", n)
	}
	ps := payloads(t, pool, ctx)
	// vectors from Python events.on_rows_changed: sha1("")[:8]=da39a3ee, sha1("a@b.c")[:8]
	want0 := "rowchg:5:email:da39a3ee->" + sha8hex("a@b.c") + ":1700000000000"
	want1 := "rowchg:6:n:" + sha8hex("1.5") + "->" + sha8hex("True") + ":"
	if ps[0].FireKey != want0 || ps[1].FireKey != want1 {
		t.Fatalf("keys %q %q want %q %q", ps[0].FireKey, ps[1].FireKey, want0, want1)
	}
	if ps[0].FireSource != "row_changed" || ps[0].TriggerID != "c1" || ps[0].Workspace != emWS || ps[0].DryRun ||
		len(ps[0].RawTargets) != 1 || ps[0].RawTargets[0]["workbook_id"] != "wb-1" || ps[0].RawTargets[0]["row_id"] != "5" {
		t.Fatalf("payload %+v", ps[0])
	}
}

func TestWatchFieldsFilter(t *testing.T) {
	pool, ctx := emSetup(t)
	emTrigger(t, pool, ctx, emWS, "w-all", "on_row_changed", `{}`, `[]`, true)
	emTrigger(t, pool, ctx, emWS, "w-email", "on_row_changed", `{"watch_fields":["email"]}`, `[]`, true)
	emTrigger(t, pool, ctx, emWS, "w-other-wb", "on_row_changed", `{}`, `["wb-9"]`, true)
	emTrigger(t, pool, ctx, emWS, "w-bad", "on_row_changed", `{"watch_fields":{"a":1}}`, `[]`, true) // malformed: fail closed
	var n int
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) (err error) {
		n, err = newE(true).RowsChanged(ctx, tx, emWS, "wb-1", []automationhook.Change{
			{RowID: 1, Field: "email", Old: "a", New: "b"}, {RowID: 1, Field: "phone", Old: "a", New: "b"}})
		return
	})
	if n != 3 { // w-all x2 + w-email x1
		t.Fatalf("n=%d", n)
	}
}

func TestSignalMatchesMapLeadToScopedRows(t *testing.T) {
	pool, ctx := emSetup(t)
	emRows(t, pool, ctx, emWS, map[string][]int{"wb-a": {7}, "wb-b": {7, 8}})
	emTrigger(t, pool, ctx, emWS, "s-scoped", "on_signal", `{}`, `["wb-b"]`, true)
	var n int
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) (err error) {
		n, err = newE(true).SignalsInserted(ctx, tx, emWS, []automationhook.SignalRef{{PK: "p1", Type: "hiring", LeadID: lead(7)}})
		return
	})
	ps := payloads(t, pool, ctx)
	if n != 1 || len(ps) != 1 || len(ps[0].RawTargets) != 1 || ps[0].RawTargets[0]["workbook_id"] != "wb-b" || ps[0].FireKey != "signal:p1" || ps[0].FireSource != "signal" {
		t.Fatalf("n=%d %+v", n, ps)
	}
}

func TestSignalNoTargetsNoJob(t *testing.T) {
	pool, ctx := emSetup(t)
	emRows(t, pool, ctx, emWS, map[string][]int{"wb-a": {7}})
	emTrigger(t, pool, ctx, emWS, "s1", "on_signal", `{}`, `["wb-zzz"]`, true)
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) error {
		n, err := newE(true).SignalsInserted(ctx, tx, emWS, []automationhook.SignalRef{{PK: "p", Type: "x", LeadID: lead(7)}, {PK: "q", Type: "x", LeadID: lead(1234)}})
		if n != 0 {
			t.Errorf("n=%d", n)
		}
		return err
	})
	if got := jobs(t, pool, ctx); len(got) != 0 {
		t.Fatalf("jobs %v", got)
	}
}

// MUST-FIX (wave1): malformed signal_types fails closed; Python fires nothing for them.
func TestSignalTypesMalformedFailsClosed(t *testing.T) {
	pool, ctx := emSetup(t)
	emRows(t, pool, ctx, emWS, map[string][]int{"wb-a": {7}})
	for i, cfg := range []string{`{"signal_types":"hiring"}`, `{"signal_types":5}`, `{"signal_types":{"a":1}}`, `{"signal_types":[["x"]]}`, `{"signal_types":[1,2]}`, `[1]`, `"x"`} {
		emTrigger(t, pool, ctx, emWS, "bad"+string(rune('a'+i)), "on_signal", cfg, `[]`, true)
	}
	// falsy values behave as "no filter" (Python `or []`), so exactly these two fire
	emTrigger(t, pool, ctx, emWS, "ok-null", "on_signal", `{"signal_types":null}`, `[]`, true)
	emTrigger(t, pool, ctx, emWS, "ok-empty", "on_signal", `{"signal_types":[]}`, `[]`, true)
	var n int
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) (err error) {
		n, err = newE(true).SignalsInserted(ctx, tx, emWS, []automationhook.SignalRef{{PK: "p", Type: "hiring", LeadID: lead(7)}})
		return
	})
	if n != 2 {
		t.Fatalf("n=%d jobs=%v", n, payloads(t, pool, ctx))
	}
}

func TestAudienceEnterExit(t *testing.T) {
	pool, ctx := emSetup(t)
	emRows(t, pool, ctx, emWS, map[string][]int{"wb-a": {7}})
	emTrigger(t, pool, ctx, emWS, "a-in", "on_audience_enter", `{}`, `[]`, true)
	emTrigger(t, pool, ctx, emWS, "a-in-other", "on_audience_enter", `{"audience_ids":["aud-2"]}`, `[]`, true)
	emTrigger(t, pool, ctx, emWS, "a-out", "on_audience_exit", `{"audience_ids":["aud-1"]}`, `[]`, true)
	var n int
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) (err error) {
		n, err = newE(true).AudienceEvents(ctx, tx, emWS, []automationhook.AudienceEvent{
			{ID: 11, Type: "entered", AudienceID: "aud-1", LeadID: 7},
			{ID: 12, Type: "exited", AudienceID: "aud-1", LeadID: 7},
			{ID: 13, Type: "exited", AudienceID: "aud-1", LeadID: 999}, // no rows
			{ID: 14, Type: "weird", AudienceID: "aud-1", LeadID: 7},    // unknown type: no rules
		})
		return
	})
	ps := payloads(t, pool, ctx)
	if n != 2 || len(ps) != 2 {
		t.Fatalf("n=%d %+v", n, ps)
	}
	if ps[0].FireKey != "audience:11:entered" || ps[0].FireSource != "audience_entered" || ps[0].TriggerID != "a-in" ||
		ps[1].FireKey != "audience:12:exited" || ps[1].FireSource != "audience_exited" || ps[1].TriggerID != "a-out" {
		t.Fatalf("%+v", ps)
	}
}

func TestExecutorColumnFollowsPolicy(t *testing.T) {
	pool, ctx := emSetup(t)
	emTrigger(t, pool, ctx, emWS, "x1", "on_row_changed", `{}`, `[]`, true)
	emTrigger(t, pool, ctx, emWS, "x2", "on_row_added", `{}`, `[]`, true)
	h := automationhook.NewWithExecutor(true, automationhook.CohortExecutor(true, true, map[string]bool{emWS: true}), slog.Default())
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) error {
		if _, err := h.RowsChanged(ctx, tx, emWS, "wb", []automationhook.Change{{RowID: 1, Field: "f", Old: 1, New: 2}}); err != nil {
			return err
		}
		_, err := h.RowsAdded(ctx, tx, emWS, "wb", []int64{1})
		return err
	})
	got := jobs(t, pool, ctx)
	if len(got) != 2 || got[0].Executor != "go" || got[1].Executor != "go" {
		t.Fatalf("%+v", got)
	}
	// cohort miss, flag off, and nonsense executor all land on python
	for _, ex := range []func(string) string{
		automationhook.CohortExecutor(true, true, map[string]bool{"other": true}),
		automationhook.CohortExecutor(true, false, map[string]bool{emWS: true}),
		automationhook.CohortExecutor(false, true, map[string]bool{emWS: true}),
		func(string) string { return "rm -rf" },
	} {
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE type='trigger_eval'`)
		h := automationhook.NewWithExecutor(true, ex, slog.Default())
		inTx(t, pool, ctx, emWS, func(tx pgx.Tx) error { _, err := h.RowsAdded(ctx, tx, emWS, "wb", []int64{1}); return err })
		if g := jobs(t, pool, ctx); len(g) != 1 || g[0].Executor != "python" {
			t.Fatalf("%+v", g)
		}
	}
}

func TestDisabledIsNoop(t *testing.T) {
	pool, ctx := emSetup(t)
	emRows(t, pool, ctx, emWS, map[string][]int{"wb-a": {7}})
	for _, typ := range []string{"on_row_changed", "on_signal", "on_audience_enter"} {
		emTrigger(t, pool, ctx, emWS, "d-"+typ, typ, `{}`, `[]`, true)
	}
	h := newE(false)
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) error {
		a, _ := h.RowsChanged(ctx, tx, emWS, "wb-a", []automationhook.Change{{RowID: 1, Field: "f"}})
		b, _ := h.SignalsInserted(ctx, tx, emWS, []automationhook.SignalRef{{PK: "p", Type: "t", LeadID: lead(7)}})
		c, _ := h.AudienceEvents(ctx, tx, emWS, []automationhook.AudienceEvent{{ID: 1, Type: "entered", LeadID: 7}})
		if a+b+c != 0 {
			t.Errorf("%d %d %d", a, b, c)
		}
		return nil
	})
	if g := jobs(t, pool, ctx); len(g) != 0 {
		t.Fatalf("%v", g)
	}
}

func TestErrorSwallowedInSavepoint(t *testing.T) {
	pool, ctx := emSetup(t)
	emRows(t, pool, ctx, emWS, map[string][]int{"wb-a": {7}})
	emTrigger(t, pool, ctx, emWS, "e1", "on_row_changed", `{}`, `[]`, true)
	emTrigger(t, pool, ctx, emWS, "e2", "on_signal", `{}`, `[]`, true)
	emTrigger(t, pool, ctx, emWS, "e3", "on_audience_enter", `{}`, `[]`, true)
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION p08t8_boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'boom'; END $$`,
		`DROP TRIGGER IF EXISTS p08t8_fail ON jobs`,
		`CREATE TRIGGER p08t8_fail BEFORE INSERT ON jobs FOR EACH ROW WHEN (NEW.type = 'trigger_eval') EXECUTE FUNCTION p08t8_boom()`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = owner.Exec(ctx, `DROP TRIGGER IF EXISTS p08t8_fail ON jobs`)
		_, _ = owner.Exec(ctx, `DROP FUNCTION IF EXISTS p08t8_boom()`)
	}()
	h := newE(true)
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) error {
		a, err1 := h.RowsChanged(ctx, tx, emWS, "wb-a", []automationhook.Change{{RowID: 1, Field: "f", Old: 1, New: 2}})
		b, err2 := h.SignalsInserted(ctx, tx, emWS, []automationhook.SignalRef{{PK: "p", Type: "t", LeadID: lead(7)}})
		c, err3 := h.AudienceEvents(ctx, tx, emWS, []automationhook.AudienceEvent{{ID: 1, Type: "entered", LeadID: 7}})
		if a+b+c != 0 || err1 != nil || err2 != nil || err3 != nil {
			t.Errorf("%d %d %d %v %v %v", a, b, c, err1, err2, err3)
		}
		// caller's tx must still be usable
		var one int
		return tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
	})
}

func TestFanOutCap(t *testing.T) {
	pool, ctx := emSetup(t)
	emRows(t, pool, ctx, emWS, map[string][]int{"wb-a": {7, 7, 7, 7, 7}})
	emTrigger(t, pool, ctx, emWS, "f1", "on_signal", `{}`, `[]`, true)
	emTrigger(t, pool, ctx, emWS, "f2", "on_audience_enter", `{}`, `[]`, true)
	h := automationhook.NewWithOptions(true, nil, slog.Default(), 3)
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) error {
		if _, err := h.SignalsInserted(ctx, tx, emWS, []automationhook.SignalRef{{PK: "p", Type: "t", LeadID: lead(7)}}); err != nil {
			return err
		}
		_, err := h.AudienceEvents(ctx, tx, emWS, []automationhook.AudienceEvent{{ID: 1, Type: "entered", LeadID: 7}})
		return err
	})
	for _, p := range payloads(t, pool, ctx) {
		if len(p.RawTargets) != 3 {
			t.Fatalf("%s targets=%d", p.TriggerID, len(p.RawTargets))
		}
	}
}

func TestTenantScoped(t *testing.T) {
	pool, ctx := emSetup(t)
	emRows(t, pool, ctx, "ws-ah-emit2", map[string][]int{"wb-o": {7}})
	emTrigger(t, pool, ctx, "ws-ah-emit2", "o1", "on_signal", `{}`, `[]`, true)
	emTrigger(t, pool, ctx, "ws-ah-emit2", "o2", "on_row_changed", `{}`, `[]`, true)
	emRows(t, pool, ctx, emWS, map[string][]int{"wb-a": {7}})
	h := newE(true)
	inTx(t, pool, ctx, emWS, func(tx pgx.Tx) error {
		a, _ := h.RowsChanged(ctx, tx, emWS, "wb-o", []automationhook.Change{{RowID: 1, Field: "f", Old: 1, New: 2}})
		b, _ := h.SignalsInserted(ctx, tx, emWS, []automationhook.SignalRef{{PK: "p", Type: "t", LeadID: lead(7)}})
		if a+b != 0 {
			t.Errorf("%d %d", a, b)
		}
		return nil
	})
	if g := jobs(t, pool, ctx); len(g) != 0 {
		t.Fatalf("%v", g)
	}
}

func sha8hex(s string) string { h := sha1.Sum([]byte(s)); return hex.EncodeToString(h[:])[:8] }
