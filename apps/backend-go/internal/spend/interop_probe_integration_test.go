//go:build integration

package spend_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// TestInteropReserve is driven by legacy/python/tests/test_go_spend_interop.py:
// it reads a reservation request from the file named by SPEND_PROBE_INPUT,
// reserves it, and writes the admission plus the Go-computed attempt key to
// SPEND_PROBE_OUT. Skipped (not a real test) when the variable is unset.
func TestInteropReserve(t *testing.T) {
	inPath, outPath := os.Getenv("SPEND_PROBE_INPUT"), os.Getenv("SPEND_PROBE_OUT")
	if inPath == "" || outPath == "" {
		t.Skip("SPEND_PROBE_INPUT/OUT not set (driven by the Python interop test)")
	}
	raw, err := os.ReadFile(inPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	in := v.(map[string]any)
	str := func(k string) string { return in[k].(string) }
	num := func(k string) int64 { return in[k].(int64) }
	r := spend.Reservation{WorkspaceID: str("workspace_id"), WorkbookID: str("workbook_id"), RunID: "job:" + str("job_id"),
		RowIdentity: str("row_identity"), ColumnID: str("column_id"), Provider: str("provider"),
		ExposureMicroUSD: num("exposure"), CellLimitMicroUSD: num("cell_limit"),
		CostBasis: in["cost_basis"].(map[string]any), Operation: in["operation"].(map[string]any)}
	jobID, _ := json.Number(str("job_id")).Int64()
	r.AttemptKey = spend.AttemptKey(r.WorkspaceID, r.WorkbookID, jobID, r.RowIdentity, r.ColumnID, r.Provider)

	pool := testpg.NewAppPool(t, 2)
	var a spend.Admission
	if err := tenancy.InWorkspace(context.Background(), pool, r.WorkspaceID, func(tx pgx.Tx) error {
		var err error
		a, err = spend.Reserve(context.Background(), tx, r)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{"ok": a.OK, "reason": a.Reason, "id": a.ID, "status": a.Status,
		"contract_hash": a.ContractHash, "reused": a.Reused, "attempt_key": r.AttemptKey})
	if err := os.WriteFile(outPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
}
