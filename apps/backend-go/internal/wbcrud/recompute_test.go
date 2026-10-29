package wbcrud_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// TestRecomputeUnavailableOnlyWhenRoutesDisabled: with platform billing on, the
// recompute is "unavailable" only when no billing-aware run starter is wired
// (GO_RUN_ROUTES_ENABLED false); with one it delegates to it and returns its answer.
func TestRecomputeUnavailableOnlyWhenRoutesDisabled(t *testing.T) {
	ctx := context.Background()
	off := &wbcrud.Recomputer{BillingEnabled: true}
	res, g, err := off.Start(ctx, nil, "ws", wbcrud.Workbook{}, []any{"c"}, []int64{1}, nil)
	if st, _ := res.Get("status"); err != nil || g != nil || st != "unavailable" {
		t.Fatalf("billing on, routes off: %v %v %v", res, g, err)
	}
	called := 0
	want := wbcrud.NewObj()
	want.Set("status", "started")
	on := &wbcrud.Recomputer{BillingEnabled: true, Run: func(_ context.Context, _ pgx.Tx, ws string, _ wbcrud.Workbook, cols []any, rows []int64, rc *wbcrud.Obj) (*wbcrud.Obj, *wbcrud.GuardError, error) {
		called++
		if ws != "ws" || len(cols) != 1 || len(rows) != 1 || rc != nil {
			t.Errorf("args: %q %v %v %v", ws, cols, rows, rc)
		}
		return want, nil, nil
	}}
	res, g, err = on.Start(ctx, nil, "ws", wbcrud.Workbook{}, []any{"c"}, []int64{1}, nil)
	if err != nil || g != nil || res != want || called != 1 {
		t.Fatalf("billing on, routes on: %v %v %v called=%d", res, g, err, called)
	}
}
