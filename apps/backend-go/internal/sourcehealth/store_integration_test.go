//go:build integration

package sourcehealth_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcehealth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const prefix = "p14t12-"

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	ctx := context.Background()
	clean := func() { _, _ = pool.Exec(ctx, `DELETE FROM source_health WHERE name LIKE 'p14t12-%'`) }
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func inTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, o interface{ Get(string) (any, bool) }, k string) any {
	t.Helper()
	v, _ := o.Get(k)
	return v
}

func TestGlobalTableNoRLSAndGrants(t *testing.T) {
	pool, ctx := setup(t)
	var rls bool
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity OR relforcerowsecurity FROM pg_class WHERE oid = 'public.source_health'::regclass`).Scan(&rls); err != nil {
		t.Fatal(err)
	}
	if rls {
		t.Fatal("source_health must not have RLS")
	}
	var sel, ins, upd bool
	if err := pool.QueryRow(ctx, `SELECT has_table_privilege(current_user,'public.source_health','SELECT'),
		has_table_privilege(current_user,'public.source_health','INSERT'), has_table_privilege(current_user,'public.source_health','UPDATE')`).Scan(&sel, &ins, &upd); err != nil {
		t.Fatal(err)
	}
	if !sel || !ins || !upd {
		t.Fatalf("app role grants select=%v insert=%v update=%v", sel, ins, upd)
	}
	// Works with no workspace GUC set at all.
	inTx(t, ctx, pool, func(tx pgx.Tx) error { _, err := sourcehealth.Map(ctx, tx); return err })
}

func TestApplyRunPersistsAndSummary(t *testing.T) {
	pool, ctx := setup(t)
	now := time.Date(2026, 8, 29, 3, 0, 0, 123456000, time.UTC)
	inTx(t, ctx, pool, func(tx pgx.Tx) error {
		return sourcehealth.ApplyRun(ctx, tx, map[string]int{prefix + "b": 0, prefix + "a": 4}, false, now)
	})
	var sum *sourcehealth.Obj
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) {
		sum, err = sourcehealth.Summary(ctx, tx, sourcehealth.Flags{Enforce: true})
		return
	})
	if !reflect.DeepEqual(sum.Keys(), []string{"enforce", "probe_enabled", "total", "by_state", "sources"}) {
		t.Fatalf("keys %v", sum.Keys())
	}
	if get(t, sum, "enforce") != true || get(t, sum, "probe_enabled") != false {
		t.Fatal("flags")
	}
	// Other suites may leave rows: locate ours.
	var ours []*sourcehealth.Obj
	for _, s := range get(t, sum, "sources").([]any) {
		o := s.(*sourcehealth.Obj)
		if strings.HasPrefix(get(t, o, "name").(string), prefix) {
			ours = append(ours, o)
		}
	}
	if len(ours) != 2 || get(t, ours[0], "name") != prefix+"a" || get(t, ours[1], "name") != prefix+"b" {
		t.Fatalf("ordered by name: %v", ours)
	}
	if get(t, ours[0], "last_probe_at") != "2026-08-29T03:00:00.123456" || get(t, ours[0], "ewma_yield") != 4.0 || get(t, ours[0], "probes") != 1 {
		t.Fatalf("row a %v", ours[0])
	}
	if get(t, ours[1], "consecutive_zero") != 1 || get(t, ours[1], "last_ok_at") != nil {
		t.Fatalf("row b %v", ours[1])
	}
	if u, ok := get(t, ours[0], "updated_at").(string); !ok || u == "" {
		t.Fatalf("updated_at %v", get(t, ours[0], "updated_at"))
	}
}

func TestSummaryShapeAndByState(t *testing.T) {
	pool, ctx := setup(t)
	now := time.Now().UTC()
	inTx(t, ctx, pool, func(tx pgx.Tx) error {
		return sourcehealth.ApplyRun(ctx, tx, map[string]int{prefix + "x": 1, prefix + "y": 1}, false, now)
	})
	// drive y to degraded
	for i := 0; i < 3; i++ {
		inTx(t, ctx, pool, func(tx pgx.Tx) error {
			return sourcehealth.ApplyRun(ctx, tx, map[string]int{prefix + "y": 0}, false, now)
		})
	}
	var m map[string]*sourcehealth.Obj
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { m, err = sourcehealth.Map(ctx, tx); return })
	if get(t, m[prefix+"x"], "state") != "healthy" || get(t, m[prefix+"y"], "state") != "degraded" {
		t.Fatalf("states x=%v y=%v", get(t, m[prefix+"x"], "state"), get(t, m[prefix+"y"], "state"))
	}
	var sum *sourcehealth.Obj
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { sum, err = sourcehealth.Summary(ctx, tx, sourcehealth.Flags{}); return })
	b, _ := json.Marshal(sum)
	var dec struct {
		Total   int            `json:"total"`
		ByState map[string]int `json:"by_state"`
		Sources []any          `json:"sources"`
	}
	if err := json.Unmarshal(b, &dec); err != nil {
		t.Fatal(err)
	}
	sum2 := 0
	for _, n := range dec.ByState {
		sum2 += n
	}
	if dec.Total != len(dec.Sources) || sum2 != dec.Total || dec.ByState["degraded"] < 1 || dec.ByState["healthy"] < 1 {
		t.Fatalf("summary %s", b)
	}
}

func TestManualEnableClearsDisabledAndPins(t *testing.T) {
	pool, ctx := setup(t)
	now := time.Now().UTC()
	name := prefix + "me"
	for i := 0; i < 6; i++ {
		inTx(t, ctx, pool, func(tx pgx.Tx) error {
			return sourcehealth.ApplyRun(ctx, tx, map[string]int{name: 0}, false, now)
		})
	}
	var m map[string]*sourcehealth.Obj
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { m, err = sourcehealth.Map(ctx, tx); return })
	if get(t, m[name], "state") != "auto_disabled" || get(t, m[name], "disabled_reason") != "zero_yield" {
		t.Fatalf("setup %v", m[name])
	}
	var o *sourcehealth.Obj
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { o, err = sourcehealth.ManualEnable(ctx, tx, name); return })
	if get(t, o, "state") != "healthy" || get(t, o, "manual_override") != true || get(t, o, "disabled_at") != nil ||
		get(t, o, "disabled_reason") != nil || get(t, o, "consecutive_zero") != 0 || get(t, o, "probes") != 6 {
		t.Fatalf("after enable %v", o)
	}
	// get-or-create for an unknown source
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { o, err = sourcehealth.ManualEnable(ctx, tx, prefix+"new"); return })
	if get(t, o, "manual_override") != true || get(t, o, "state") != "healthy" || get(t, o, "probes") != 0 || get(t, o, "ewma_yield") != nil {
		t.Fatalf("created %v", o)
	}
	// pinned row never auto-disables
	for i := 0; i < 8; i++ {
		inTx(t, ctx, pool, func(tx pgx.Tx) error {
			return sourcehealth.ApplyRun(ctx, tx, map[string]int{name: 0}, false, now)
		})
	}
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { m, err = sourcehealth.Map(ctx, tx); return })
	if get(t, m[name], "state") != "degraded" {
		t.Fatalf("pinned state %v", get(t, m[name], "state"))
	}
}

func TestResetBaseline(t *testing.T) {
	pool, ctx := setup(t)
	now := time.Now().UTC()
	name := prefix + "rs"
	var o *sourcehealth.Obj
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { o, err = sourcehealth.Reset(ctx, tx, name); return })
	if o != nil {
		t.Fatalf("reset of missing row must be nil, got %v", o)
	}
	for i := 0; i < 7; i++ {
		inTx(t, ctx, pool, func(tx pgx.Tx) error {
			return sourcehealth.ApplyRun(ctx, tx, map[string]int{name: 0}, false, now)
		})
	}
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { o, err = sourcehealth.ManualEnable(ctx, tx, name); return })
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { o, err = sourcehealth.Reset(ctx, tx, name); return })
	if o == nil || get(t, o, "state") != "healthy" || get(t, o, "consecutive_zero") != 0 || get(t, o, "consecutive_nonzero") != 0 ||
		get(t, o, "manual_override") != false || get(t, o, "disabled_at") != nil || get(t, o, "probes") != 7 || get(t, o, "last_yield") != 0 {
		t.Fatalf("reset %v", o)
	}
}

func TestDisabledNamesEmptyWhenEnforceOff(t *testing.T) {
	pool, ctx := setup(t)
	now := time.Now().UTC()
	name := prefix + "dn"
	for i := 0; i < 6; i++ {
		inTx(t, ctx, pool, func(tx pgx.Tx) error {
			return sourcehealth.ApplyRun(ctx, tx, map[string]int{name: 0}, false, now)
		})
	}
	var off, on map[string]bool
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) {
		if off, err = sourcehealth.DisabledNames(ctx, tx, false); err != nil {
			return
		}
		on, err = sourcehealth.DisabledNames(ctx, tx, true)
		return
	})
	if len(off) != 0 || !on[name] {
		t.Fatalf("off=%v on has=%v", off, on[name])
	}
	var dis bool
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { dis, err = sourcehealth.IsDisabled(ctx, tx, true, name); return })
	if !dis {
		t.Fatal("IsDisabled enforce on")
	}
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { dis, err = sourcehealth.IsDisabled(ctx, tx, false, name); return })
	if dis {
		t.Fatal("IsDisabled must be false when enforce off")
	}
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { dis, err = sourcehealth.IsDisabled(ctx, tx, true, prefix+"missing"); return })
	if dis {
		t.Fatal("missing row is fail-open")
	}
}

func TestApplyRunAtomic(t *testing.T) {
	pool, ctx := setup(t)
	now := time.Now().UTC()
	tooLong := prefix + strings.Repeat("z", 120) // name is varchar(100): this row fails
	err := sourcehealth.ApplyRunPool(ctx, pool, map[string]int{prefix + "ok1": 3, prefix + "ok2": 0, tooLong: 1}, false, now)
	if err == nil {
		t.Fatal("expected failure")
	}
	var n int
	if e := pool.QueryRow(ctx, `SELECT count(*) FROM source_health WHERE name LIKE 'p14t12-%'`).Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 0 {
		t.Fatalf("partial run persisted %d rows", n)
	}
}

func TestApplyRunConcurrentSafe(t *testing.T) {
	pool, ctx := setup(t)
	now := time.Now().UTC()
	const workers, rounds = 12, 5
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				// opposite insertion orders; the implementation must lock in a stable order
				yields := map[string]int{prefix + "c1": 0, prefix + "c2": 0, prefix + "c3": 1}
				if err := sourcehealth.ApplyRunPool(ctx, pool, yields, false, now); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var m map[string]*sourcehealth.Obj
	inTx(t, ctx, pool, func(tx pgx.Tx) (err error) { m, err = sourcehealth.Map(ctx, tx); return })
	for _, n := range []string{"c1", "c2", "c3"} {
		if get(t, m[prefix+n], "probes") != workers*rounds {
			t.Fatalf("%s probes %v want %d (lost update)", n, get(t, m[prefix+n], "probes"), workers*rounds)
		}
	}
	// 60 zero runs: counters and state advanced exactly
	if get(t, m[prefix+"c1"], "consecutive_zero") != workers*rounds || get(t, m[prefix+"c1"], "state") != "auto_disabled" ||
		get(t, m[prefix+"c3"], "consecutive_nonzero") != workers*rounds {
		t.Fatalf("c1 %v c3 %v", m[prefix+"c1"], m[prefix+"c3"])
	}
}
