//go:build integration

package sourcewb

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// seedCollectWorkbook seeds the collect_basic workbook (draft) without its prior collection job.
func seedCollectWorkbook(t *testing.T, owner *pgxpool.Pool) (ws, wb string) {
	t.Helper()
	_, raws := loadVectors(t)
	ctx := context.Background()
	for _, raw := range raws {
		var sc scenario
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatal(err)
		}
		if sc.Name != "collect_basic" {
			continue
		}
		for _, tb := range cleanOrder {
			if _, err := owner.Exec(ctx, "DELETE FROM "+tb+" WHERE workspace_id = $1", sc.WorkspaceID); err != nil {
				t.Fatal(err)
			}
		}
		for _, r := range sc.Seed["workbooks"] {
			if _, err := owner.Exec(ctx, "INSERT INTO workbooks SELECT * FROM json_populate_record(NULL::workbooks, $1::json)", string(r)); err != nil {
				t.Fatal(err)
			}
		}
		return sc.WorkspaceID, sc.WorkbookID
	}
	t.Fatal("collect_basic scenario missing")
	return
}

func wbStatus(t *testing.T, owner *pgxpool.Pool, wb string) string {
	t.Helper()
	var st string
	if err := owner.QueryRow(context.Background(), "SELECT status FROM workbooks WHERE id = $1", wb).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func testGate() SplitGate {
	return SplitGate{Validate: func(*wbcrud.Obj) (bool, string) { return true, "" },
		Fit: func(_, _ *wbcrud.Obj) (*wbcrud.Obj, error) { return nil, nil }}
}

type cancellingCollector struct{ cancel context.CancelFunc }

func (c cancellingCollector) Submit(ctx context.Context, _, _ string) (string, error) {
	c.cancel()
	return "", ctx.Err()
}

// Review 3b L1: a job cancelled mid-run must not leave the workbook "running".
func TestCancelledRunLeavesATerminalCancelledStatus(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	owner := ownerPool(t)
	ws, wb := seedCollectWorkbook(t, owner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := Deps{Pool: pool, Collector: cancellingCollector{cancel}, Gate: testGate()}
	_, err := Materialize(ctx, d, ws, wb, "src_1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := wbStatus(t, owner, wb); got != "cancelled" {
		t.Errorf("workbook status = %q, want cancelled", got)
	}
}

type keyedFake struct {
	keys []string
	id   string
}

func (k *keyedFake) Submit(context.Context, string, string) (string, error) {
	return "", errors.New("unkeyed Submit used")
}

func (k *keyedFake) SubmitKeyed(_ context.Context, _, _, key string) (string, error) {
	k.keys = append(k.keys, key)
	return k.id, nil
}

// Review 3b L2: every attempt of one queue job submits under the same idempotency key, so a retry reuses the
// collection the first attempt paid for.
func TestRetryUsesTheSameCollectionKeyPerJob(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	owner := ownerPool(t)
	ws, wb := seedCollectWorkbook(t, owner)
	col := &keyedFake{id: "job-sw1"}
	d := Deps{Pool: pool, Collector: col, Gate: testGate()}
	b, _ := json.Marshal(map[string]any{"workbook_id": wb, "column_id": "src_1", "workspace_id": ws})
	for i := 0; i < 2; i++ {
		if err := Handle(context.Background(), d, &queue.Claim{ID: 4242, Type: JobType, WorkspaceID: ws, Payload: b}); err != nil {
			t.Fatal(err)
		}
	}
	if len(col.keys) != 2 || col.keys[0] != col.keys[1] || col.keys[0] == "" {
		t.Fatalf("submit keys = %v, want the same non-empty key twice", col.keys)
	}
}

// The production collector returns a finished collection of the same key without starting a run (Deps.For is
// nil here: touching the runner would panic), and refuses one that is still in flight.
func TestCollectCollectorSubmitKeyedReusesFinishedCollection(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	owner := ownerPool(t)
	ws, _ := seedCollectWorkbook(t, owner)
	ctx := context.Background()
	id := KeyedJobID(ws, "source_workbook:9")
	if id == KeyedJobID(ws, "source_workbook:10") || id == KeyedJobID("other", "source_workbook:9") {
		t.Fatal("keyed ids collide")
	}
	ins := func(status string) {
		_, err := owner.Exec(ctx, `INSERT INTO collection_jobs (id, workspace_id, query, intent, intent_details, status, created_at)
			VALUES ($1,$2,'q','market_search','{}',$3,'2026-09-01T10:00:00+00:00') ON CONFLICT (id) DO UPDATE SET status = $3`, id, ws, status)
		if err != nil {
			t.Fatal(err)
		}
	}
	c := CollectCollector{Deps: &collect.Deps{Pool: pool, For: func(string) *collect.Runner { t.Fatal("runner touched"); return nil }}}
	ins("done")
	got, err := c.SubmitKeyed(ctx, ws, "q", "source_workbook:9")
	if err != nil || got != id {
		t.Fatalf("SubmitKeyed = %q, %v; want the earlier collection %q", got, err, id)
	}
	ins("running")
	if _, err := c.SubmitKeyed(ctx, ws, "q", "source_workbook:9"); !errors.Is(err, ErrCollectionInFlight) {
		t.Fatalf("in-flight err = %v", err)
	}
}
