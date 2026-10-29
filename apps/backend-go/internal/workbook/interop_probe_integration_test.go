//go:build integration

package workbook

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// TestInteropReserveCell is driven by legacy/python/tests/test_go_spend_interop.py:
// it reserves a spend attempt for {lead, column, provider} using the executor's
// own reservation builder and writes the admission to WB_PROBE_OUT.
func TestInteropReserveCell(t *testing.T) {
	inPath, outPath := os.Getenv("WB_PROBE_INPUT"), os.Getenv("WB_PROBE_OUT")
	if inPath == "" || outPath == "" {
		t.Skip("WB_PROBE_INPUT/OUT not set (driven by the Python interop test)")
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
	jobID, _ := json.Number(in["job_id"].(string)).Int64()
	rs := &runState{claim: &queue.Claim{ID: jobID},
		p: &runPayload{WorkspaceID: in["workspace_id"].(string), WorkbookID: in["workbook_id"].(string)}}
	cfg := in["column"].(map[string]any)
	col := column{cfg: cfg, ID: cfg["id"].(string)}
	r := rs.buildReservation(in["provider"].(string), in["lead"].(map[string]any), col,
		in["row_identity"].(string), in["exposure"].(int64), in["cell_limit"].(int64))

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
