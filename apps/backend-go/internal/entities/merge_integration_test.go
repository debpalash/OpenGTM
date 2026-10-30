//go:build integration

package entities

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const vecWS = "ws-vec"

var cleanOrder = []string{"entity_review_pairs", "entity_merge_log", "workbook_rows", "person_employments", "person_entities", "watch_subscriptions", "entity_blocking_keys", "company_identifiers", "company_entities", "workbooks"}

func clean(t testing.TB, pool *pgxpool.Pool, ws string) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		for _, tbl := range cleanOrder {
			if _, err := tx.Exec(ctx, `DELETE FROM `+tbl+` WHERE workspace_id=$1`, ws); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// seedVec inserts the vector seed (the Python generator's input) into ws-vec and removes it with the test.
func seedVec(t *testing.T, pool *pgxpool.Pool, v vectors) {
	t.Helper()
	ctx := context.Background()
	clean(t, pool, vecWS)
	t.Cleanup(func() { clean(t, pool, vecWS) })
	for _, tbl := range v.SeedOrder { // recorded serial ids sit below testpg.SeqFloor
		testpg.FreeRecordIDs(t, tbl, v.Seed[tbl]...)
	}
	err := tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
		for _, tbl := range v.SeedOrder {
			for _, row := range v.Seed[tbl] {
				if _, err := tx.Exec(ctx, `INSERT INTO `+tbl+` SELECT * FROM json_populate_record(NULL::`+tbl+`, $1::json)`, string(row)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// dump runs the generator's DUMP_SQL and normalises set-ordered `sources` lists.
func dump(t *testing.T, pool *pgxpool.Pool, v vectors) map[string]any {
	t.Helper()
	ctx := context.Background()
	var raw string
	if err := tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error { return tx.QueryRow(ctx, v.DumpSQL).Scan(&raw) }); err != nil {
		t.Fatal(err)
	}
	return normalize(t, []byte(raw))
}

func normalize(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	stripCanonicalMetadata(m)
	for _, e := range m["company_entities"].([]any) {
		srcs := e.(map[string]any)["sources"].([]any)
		sort.Slice(srcs, func(i, j int) bool { return srcs[i].(string) < srcs[j].(string) })
	}
	return m
}

// These recordings assert the historical graph contract. Accepted facts and
// their merge undo data have separate coverage in internal/records.
func stripCanonicalMetadata(value any) {
	switch v := value.(type) {
	case map[string]any:
		if _, ok := v["workspace_id"]; ok {
			if _, company := v["canonical_name"]; company {
				delete(v, "accepted_fields")
				delete(v, "record_version")
				delete(v, "record_name_key")
			}
			if _, person := v["full_name"]; person {
				delete(v, "accepted_fields")
				delete(v, "record_version")
			}
			if _, employment := v["company_key"]; employment {
				delete(v, "accepted_fields")
				delete(v, "ended_at")
			}
		}
		if snapshot, ok := v["snapshot"].(map[string]any); ok {
			delete(snapshot, "shared_records")
		}
		for _, child := range v {
			stripCanonicalMetadata(child)
		}
	case []any:
		for _, child := range v {
			stripCanonicalMetadata(child)
		}
	}
}

func want(t *testing.T, state map[string]json.RawMessage) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(state)
	return normalize(t, raw)
}

func assertState(t *testing.T, got, want map[string]any) {
	t.Helper()
	for k := range want {
		if !reflect.DeepEqual(got[k], want[k]) {
			g, _ := json.MarshalIndent(got[k], "", " ")
			w, _ := json.MarshalIndent(want[k], "", " ")
			t.Errorf("%s differs\n got: %s\nwant: %s", k, g, w)
		}
	}
}

func opCode(err error) string {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Code
	}
	return "<" + err.Error() + ">"
}

func TestMergeThenPythonSplitRestores(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := loadVectors(t)
	seedVec(t, pool, v)
	st := &Store{Pool: pool}
	ctx := context.Background()
	res, err := st.Merge(ctx, vecWS, "e-keep", "e-gone", "manual")
	if err != nil {
		t.Fatal(err)
	}
	var wantRes MergeResult
	_ = json.Unmarshal(v.Python.Merge, &wantRes)
	if !reflect.DeepEqual(res, wantRes) {
		t.Errorf("merge result %+v, python %+v", res, wantRes)
	}
	// State and merge-log snapshot (the input Python's split_entity consumes) equal what Python wrote.
	assertState(t, dump(t, pool, v), want(t, v.Python.AfterMerge))

	// Go split restores exactly what Python's split restores, including a later observation on the survivor.
	if err := tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, v.PostMergeSQL); return err }); err != nil {
		t.Fatal(err)
	}
	assertState(t, dump(t, pool, v), want(t, v.Python.AfterLateObservation))
	var logID int64
	_ = tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM entity_merge_log WHERE workspace_id=$1`, vecWS).Scan(&logID)
	})
	sres, err := st.Split(ctx, vecWS, logID)
	if err != nil {
		t.Fatal(err)
	}
	var wantSplit SplitResult
	_ = json.Unmarshal(v.Python.Split, &wantSplit)
	if sres != wantSplit {
		t.Errorf("split result %+v, python %+v", sres, wantSplit)
	}
	assertState(t, dump(t, pool, v), want(t, v.Python.AfterSplit))
	if _, err := st.Split(ctx, vecWS, logID); opCode(err) != v.Python.Errors["split_twice"].Error {
		t.Errorf("split twice: %v", err)
	}
}

func TestPythonMergeThenGoSplitRestores(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := loadVectors(t)
	seedVec(t, pool, v)
	st := &Store{Pool: pool}
	ctx := context.Background()
	if _, err := st.Merge(ctx, vecWS, "e-keep", "e-gone", "manual"); err != nil {
		t.Fatal(err)
	}
	// Replace the Go-written snapshot with the one Python wrote for the same merge.
	pyLog := v.Python.AfterMerge["entity_merge_log"]
	var logs []struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(pyLog, &logs); err != nil || len(logs) != 1 {
		t.Fatalf("python log: %v %d", err, len(logs))
	}
	var logID int64
	if err := tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE entity_merge_log SET snapshot=$1::json WHERE workspace_id=$2`, string(logs[0].Snapshot), vecWS); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, v.PostMergeSQL); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT id FROM entity_merge_log WHERE workspace_id=$1`, vecWS).Scan(&logID)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Split(ctx, vecWS, logID); err != nil {
		t.Fatal(err)
	}
	assertState(t, dump(t, pool, v), want(t, v.Python.AfterSplit))
}

func TestMergeWritesLogAndRepointsAccountsAndPeople(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := loadVectors(t)
	seedVec(t, pool, v)
	ctx := context.Background()
	st := &Store{Pool: pool}
	if _, err := st.Merge(ctx, vecWS, "e-keep", "e-gone", "because"); err != nil {
		t.Fatal(err)
	}
	err := tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
		var reason string
		var score float64
		var reverted int
		if err := tx.QueryRow(ctx, `SELECT reason, score, reverted FROM entity_merge_log WHERE workspace_id=$1`, vecWS).Scan(&reason, &score, &reverted); err != nil {
			return err
		}
		if reason != "because" || reverted != 0 || score <= 0 || score > 1 {
			t.Errorf("log reason=%q score=%v reverted=%d", reason, score, reverted)
		}
		var n int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM company_entities WHERE id='e-gone'`).Scan(&n)
		if n != 0 {
			t.Error("merged entity still exists")
		}
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM person_entities WHERE company_entity_id='e-keep'`).Scan(&n)
		if n != 1 {
			t.Errorf("people on kept = %d", n)
		}
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM person_employments WHERE company_entity_id='e-keep'`).Scan(&n)
		if n != 1 {
			t.Errorf("employments on kept = %d", n)
		}
		// Rows touched by the merge are stamped like SQLAlchemy's onupdate; the untouched row is not.
		var stamped, untouched bool
		_ = tx.QueryRow(ctx, `SELECT updated_at > '2026-08-29T10:00:00' FROM workbook_rows WHERE id=9001`).Scan(&stamped)
		_ = tx.QueryRow(ctx, `SELECT updated_at > '2026-08-29T10:00:00' FROM workbook_rows WHERE id=9004`).Scan(&untouched)
		if !stamped || untouched {
			t.Errorf("updated_at stamped=%v untouched-row-stamped=%v", stamped, untouched)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMergeSameIDError(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	st := &Store{Pool: pool}
	_, err := st.Merge(context.Background(), vecWS, "x", "x", "manual")
	if opCode(err) != "same_entity" {
		t.Fatalf("%v", err)
	}
	_, err = st.Merge(context.Background(), vecWS, "nope", "nada", "manual")
	if opCode(err) != "entity_not_found" {
		t.Fatalf("%v", err)
	}
}

func TestSplitUnknownLog400(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	st := &Store{Pool: pool}
	for _, id := range []int64{987654, -1, 1 << 40} {
		if _, err := st.Split(context.Background(), vecWS, id); opCode(err) != "merge_log_not_found_or_reverted" {
			t.Errorf("id %d: %v", id, err)
		}
	}
}

func snapshotOf(t *testing.T, pool *pgxpool.Pool, ws string) string {
	t.Helper()
	var s string
	_ = tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT coalesce(json_agg(to_jsonb(c) ORDER BY id)::text,'') FROM company_entities c WHERE workspace_id=$1`, ws).Scan(&s)
	})
	return s
}

func TestMergeCrossWorkspaceIDs400NothingWritten(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := loadVectors(t)
	seedVec(t, pool, v)
	other := "ws-vec-other"
	clean(t, pool, other)
	t.Cleanup(func() { clean(t, pool, other) })
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, pool, other, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO company_entities (id, workspace_id, canonical_name, fields, sources, observation_count, corroboration_count)
			VALUES ('e-foreign', $1, 'Foreign', '{}', '[]', 1, 0)`, other)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	beforeVec, beforeOther := snapshotOf(t, pool, vecWS), snapshotOf(t, pool, other)
	st := &Store{Pool: pool}
	// merged id owned by another workspace, then kept id owned by another workspace, in both directions.
	for _, c := range [][2]string{{"e-keep", "e-foreign"}, {"e-foreign", "e-keep"}} {
		for _, ws := range []string{vecWS, other} {
			if _, err := st.Merge(ctx, ws, c[0], c[1], "manual"); opCode(err) != "entity_not_found" {
				t.Errorf("merge %v as %s: %v", c, ws, err)
			}
		}
	}
	if snapshotOf(t, pool, vecWS) != beforeVec || snapshotOf(t, pool, other) != beforeOther {
		t.Error("a refused cross-workspace merge changed entities")
	}
	var logs int
	_ = tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM entity_merge_log WHERE workspace_id=$1`, vecWS).Scan(&logs)
	})
	if logs != 0 {
		t.Errorf("merge log rows written: %d", logs)
	}
}

func TestSplitOtherWorkspaceLog400(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := loadVectors(t)
	seedVec(t, pool, v)
	other := "ws-vec-other"
	clean(t, pool, other)
	t.Cleanup(func() { clean(t, pool, other) })
	st := &Store{Pool: pool}
	ctx := context.Background()
	if _, err := st.Merge(ctx, vecWS, "e-keep", "e-gone", "manual"); err != nil {
		t.Fatal(err)
	}
	var logID int64
	_ = tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM entity_merge_log WHERE workspace_id=$1`, vecWS).Scan(&logID)
	})
	if _, err := st.Split(ctx, other, logID); opCode(err) != "merge_log_not_found_or_reverted" {
		t.Fatalf("%v", err)
	}
	var n int
	_ = tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM company_entities WHERE id='e-gone'`).Scan(&n)
	})
	if n != 0 {
		t.Error("foreign split recreated the entity")
	}
}

// Python raises TypeError (HTTP 500) when provenance is not field -> list; Go refuses with a 400 code and writes nothing.
func TestMergeNonListFieldsRejectedNothingWritten(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := loadVectors(t)
	seedVec(t, pool, v)
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE company_entities SET fields='{"name":"Acme"}' WHERE id='e-gone'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotOf(t, pool, vecWS)
	if _, err := (&Store{Pool: pool}).Merge(ctx, vecWS, "e-keep", "e-gone", "manual"); opCode(err) != "invalid_entity_fields" {
		t.Fatalf("%v", err)
	}
	if snapshotOf(t, pool, vecWS) != before {
		t.Error("entities changed")
	}
}

func pairFor(t *testing.T, pool *pgxpool.Pool, ws, entity, cand string) int32 {
	t.Helper()
	ctx := context.Background()
	var id int32
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO entity_review_pairs (entity_id, candidate, score, reason, status, workspace_id)
			VALUES ($1,$2::json,0.8,'grey_band','pending',$3) RETURNING id`, entity, cand, ws).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func pairStatus(t *testing.T, pool *pgxpool.Pool, ws string, id int32) string {
	t.Helper()
	var s string
	_ = tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status FROM entity_review_pairs WHERE id=$1`, id).Scan(&s)
	})
	return s
}

func TestDecideMergeUsesCandidateNewEntityID(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := loadVectors(t)
	seedVec(t, pool, v)
	st := &Store{Pool: pool}
	ctx := context.Background()
	id := pairFor(t, pool, vecWS, "e-keep", `{"id":"e-other","new_entity_id":"e-gone"}`)
	res, err := st.Decide(ctx, vecWS, int64(id), "merge")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "merged" || res.Merge == nil || res.Merge.KeptID != "e-keep" || res.Merge.MergedID != "e-gone" {
		t.Fatalf("%+v", res)
	}
	if s := pairStatus(t, pool, vecWS, id); s != "merged" {
		t.Errorf("pair status %q", s)
	}
	var reason string
	_ = tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reason FROM entity_merge_log WHERE workspace_id=$1`, vecWS).Scan(&reason)
	})
	if reason != "review_confirmed" {
		t.Errorf("reason %q", reason)
	}
	// No candidate entity: untouched pair, nothing written.
	id2 := pairFor(t, pool, vecWS, "e-keep", `{"id":"e-other"}`)
	if _, err := st.Decide(ctx, vecWS, int64(id2), "merge"); !errors.Is(err, ErrNoCandidate) {
		t.Errorf("%v", err)
	}
	if s := pairStatus(t, pool, vecWS, id2); s != "pending" {
		t.Errorf("pair status %q", s)
	}
	// A merge that errors leaves the pair pending (candidate already merged away).
	id3 := pairFor(t, pool, vecWS, "e-keep", `{"new_entity_id":"e-gone"}`)
	if _, err := st.Decide(ctx, vecWS, int64(id3), "merge"); opCode(err) != "entity_not_found" {
		t.Errorf("%v", err)
	}
	if s := pairStatus(t, pool, vecWS, id3); s != "pending" {
		t.Errorf("pair status %q", s)
	}
}

func TestDecideRejectAnyOtherDecision(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := loadVectors(t)
	seedVec(t, pool, v)
	st := &Store{Pool: pool}
	for _, d := range []string{"reject", "MERGE", "", "banana"} {
		id := pairFor(t, pool, vecWS, "e-keep", `{"new_entity_id":"e-gone"}`)
		res, err := st.Decide(context.Background(), vecWS, int64(id), d)
		if err != nil || res.Status != "rejected" || res.Merge != nil {
			t.Fatalf("decision %q: %+v %v", d, res, err)
		}
		if s := pairStatus(t, pool, vecWS, id); s != "rejected" {
			t.Errorf("decision %q: pair status %q", d, s)
		}
	}
	var n int
	_ = tenancy.InWorkspace(context.Background(), pool, vecWS, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM company_entities WHERE id='e-gone'`).Scan(&n)
	})
	if n != 1 {
		t.Error("a rejection merged entities")
	}
}

func TestDecidePairOfOtherWorkspace404(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := loadVectors(t)
	seedVec(t, pool, v)
	other := "ws-vec-other"
	clean(t, pool, other)
	t.Cleanup(func() { clean(t, pool, other) })
	id := pairFor(t, pool, vecWS, "e-keep", `{"new_entity_id":"e-gone"}`)
	st := &Store{Pool: pool}
	for _, d := range []string{"merge", "reject"} {
		if _, err := st.Decide(context.Background(), other, int64(id), d); !errors.Is(err, ErrPairNotFound) {
			t.Errorf("%s: %v", d, err)
		}
	}
	if s := pairStatus(t, pool, vecWS, id); s != "pending" {
		t.Errorf("pair status %q", s)
	}
	if _, err := st.Decide(context.Background(), vecWS, 1<<40, "merge"); !errors.Is(err, ErrPairNotFound) {
		t.Errorf("%v", err)
	}
}

func TestMergeConcurrentSamePairOnce(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	v := loadVectors(t)
	seedVec(t, pool, v)
	st := &Store{Pool: pool}
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = st.Merge(context.Background(), vecWS, "e-keep", "e-gone", "manual")
		}(i)
	}
	close(start)
	wg.Wait()
	ok, missing := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case opCode(err) == "entity_not_found":
			missing++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 || missing != n-1 {
		t.Fatalf("ok=%d missing=%d", ok, missing)
	}
	var logs int
	_ = tenancy.InWorkspace(context.Background(), pool, vecWS, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM entity_merge_log WHERE workspace_id=$1`, vecWS).Scan(&logs)
	})
	if logs != 1 {
		t.Errorf("merge log rows: %d", logs)
	}
	// Observation count was summed once.
	assertState(t, dump(t, pool, v), want(t, v.Python.AfterMerge))
}

// Split and merge into the same kept entity, started together in both directions, must not deadlock.
func TestSplitRacingMergeSameKeptNoDeadlock(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	v := loadVectors(t)
	st := &Store{Pool: pool}
	ctx := context.Background()
	for round := 0; round < 15; round++ {
		seedVec(t, pool, v)
		if _, err := st.Merge(ctx, vecWS, "e-keep", "e-gone", "manual"); err != nil {
			t.Fatal(err)
		}
		var logID int64
		_ = tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM entity_merge_log WHERE workspace_id=$1`, vecWS).Scan(&logID)
		})
		start := make(chan struct{})
		var wg sync.WaitGroup
		var splitErr, mergeErr error
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, splitErr = st.Split(ctx, vecWS, logID) }()
		go func() { defer wg.Done(); <-start; _, mergeErr = st.Merge(ctx, vecWS, "e-keep", "e-other", "manual") }()
		close(start)
		wg.Wait()
		var conflict *pgconn.PgError
		guarded := errors.As(splitErr, &conflict) && conflict.Code == "40001"
		if (splitErr != nil && !guarded) || mergeErr != nil {
			t.Fatalf("round %d: split=%v merge=%v", round, splitErr, mergeErr)
		}
		// Consistent end state: e-gone restored, e-other merged away, one log reverted and one open.
		var gone, other, reverted, open int
		_ = tenancy.InWorkspace(ctx, pool, vecWS, func(tx pgx.Tx) error {
			_ = tx.QueryRow(ctx, `SELECT count(*) FROM company_entities WHERE id='e-gone'`).Scan(&gone)
			_ = tx.QueryRow(ctx, `SELECT count(*) FROM company_entities WHERE id='e-other'`).Scan(&other)
			_ = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE reverted=1), count(*) FILTER (WHERE reverted=0) FROM entity_merge_log WHERE workspace_id=$1`, vecWS).Scan(&reverted, &open)
			return nil
		})
		wantGone, wantReverted, wantOpen := 1, 1, 1
		if guarded {
			wantGone, wantReverted, wantOpen = 0, 0, 2
		}
		if gone != wantGone || other != 0 || reverted != wantReverted || open != wantOpen {
			t.Fatalf("round %d: gone=%d other=%d reverted=%d open=%d", round, gone, other, reverted, open)
		}
	}
}

// (A,B) and (B,A) merges together: sorted lock order means one wins, the other sees entity_not_found.
func TestMergeOppositeDirectionsNoDeadlock(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	v := loadVectors(t)
	st := &Store{Pool: pool}
	for round := 0; round < 15; round++ {
		seedVec(t, pool, v)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		pairs := [][2]string{{"e-keep", "e-gone"}, {"e-gone", "e-keep"}}
		for i := range pairs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = st.Merge(context.Background(), vecWS, pairs[i][0], pairs[i][1], "manual")
			}(i)
		}
		close(start)
		wg.Wait()
		okN := 0
		for _, err := range errs {
			if err == nil {
				okN++
			} else if opCode(err) != "entity_not_found" {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if okN != 1 {
			t.Fatalf("round %d: %d merges succeeded", round, okN)
		}
	}
}
