//go:build integration

package spend_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Workspace-level paid calls (no workbook, e.g. copilot people contacts) use the same
// attempt lifecycle under the reserved WorkspaceLedger id: reserve, dispatch, settle once,
// the cell limit bounds exposure, and no workbook is locked or charged.
func TestWorkspaceLedgerLifecycle(t *testing.T) {
	pool, ctx := setup(t, f(1), f(0))
	r := res("wk1", 20000, 30000)
	r.WorkbookID = spend.WorkspaceLedger
	var adm spend.Admission
	in := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := tenancy.InWorkspace(ctx, pool, ws, fn); err != nil {
			t.Fatal(err)
		}
	}
	in(func(tx pgx.Tx) (err error) { adm, err = spend.ReserveWorkspace(ctx, tx, r); return })
	if !adm.OK || adm.Status != "reserved" {
		t.Fatalf("%+v", adm)
	}
	// A second attempt of the same cell over the cell limit is refused.
	r2 := res("wk2", 20000, 30000)
	r2.WorkbookID = spend.WorkspaceLedger
	var adm2 spend.Admission
	in(func(tx pgx.Tx) (err error) { adm2, err = spend.ReserveWorkspace(ctx, tx, r2); return })
	if adm2.OK || adm2.Reason != "cell_budget" {
		t.Fatalf("%+v", adm2)
	}
	id := spend.Identity{WorkspaceID: ws, WorkbookID: spend.WorkspaceLedger, AttemptID: adm.ID, ContractHash: adm.ContractHash}
	var ok bool
	in(func(tx pgx.Tx) (err error) { ok, err = spend.Transition(ctx, tx, id, "dispatch"); return })
	if !ok {
		t.Fatal("dispatch")
	}
	var sr spend.SettleResult
	in(func(tx pgx.Tx) (err error) {
		sr, err = spend.SettleWorkspace(ctx, tx, id, 20000, "catalog_estimate", map[string]any{"ok": true})
		return
	})
	if !sr.OK {
		t.Fatalf("%+v", sr)
	}
	in(func(tx pgx.Tx) (err error) {
		sr, err = spend.SettleWorkspace(ctx, tx, id, 20000, "catalog_estimate", map[string]any{"ok": true})
		return
	})
	if !sr.OK || !sr.Reused {
		t.Fatalf("second settle %+v", sr)
	}
	// The workspace scope is not a workbook: the regular entry points refuse it, and a
	// workbook id is refused by the workspace entry points.
	in(func(tx pgx.Tx) (err error) { adm2, err = spend.Reserve(ctx, tx, r2); return })
	if adm2.OK {
		t.Fatalf("workbook reserve accepted the workspace ledger: %+v", adm2)
	}
	in(func(tx pgx.Tx) error {
		_, err := spend.ReserveWorkspace(ctx, tx, res("wk3", 1, 1))
		if err == nil {
			t.Fatal("workspace reserve accepted a workbook id")
		}
		return nil
	})
	var spent float64
	in(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT budget_spent_usd FROM workbooks WHERE id='wb1'`).Scan(&spent)
	})
	if spent != 0 {
		t.Fatalf("workbook charged %v", spent)
	}
}
