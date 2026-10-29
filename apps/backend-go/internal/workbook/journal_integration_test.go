//go:build integration

package workbook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// TestOutputJournalVectors replays output_attempts.execute_output_with_journal: a queued run
// claims the cell in the job payload before sending, reuses a recorded receipt, refuses a
// changed contract, never resends a claim without a receipt, and keys lead-store rows by lead.
// The contract hashes and the final journal equal Python's.
func TestOutputJournalVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/wbexec_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Workspace string `json:"workspace_id"`
		Workbook  string `json:"workbook_id"`
		Journal   struct {
			Steps []struct {
				Name    string `json:"name"`
				Request struct {
					ColID    string         `json:"col_id"`
					RowID    *int64         `json:"row_id"`
					LeadID   int64          `json:"lead_id"`
					ColExtra map[string]any `json:"col_extra"`
				} `json:"request"`
				Result struct {
					Value   map[string]any `json:"value"`
					Error   string         `json:"error"`
					Message string         `json:"message"`
				} `json:"result"`
				Calls    []string       `json:"calls"`
				Attempts map[string]any `json:"attempts"`
			} `json:"steps"`
			RowHash string `json:"row11_contract_hash"`
		} `json:"journal"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
	  VALUES ('run_workbook', $1::json, 'pending', 0, now() AT TIME ZONE 'UTC', 0, 3, $2, 'go')`,
		`{"workspace_id":"`+v.Workspace+`","workbook_id":"`+v.Workbook+`"}`, v.Workspace); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.ClaimNext(ctx, pool, "journal-test", 0)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	var calls []string
	rs := &runState{deps: Deps{Pool: pool}, claim: claim, p: &runPayload{WorkspaceID: v.Workspace, WorkbookID: v.Workbook},
		log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	rs.outputExec = func(_ context.Context, _ map[string]any, col column, _ int64) (outResult, error) {
		calls = append(calls, col.ID)
		if col.ID == "o2" {
			return outResult{Value: "POST 500", HasVal: true, Error: "HTTP 500: x"}, nil
		}
		return outResult{Success: true, Value: "POST 200", HasVal: true}, nil
	}
	attempts := func() map[string]any {
		var p map[string]any
		if err := pool.QueryRow(ctx, `SELECT payload::jsonb FROM jobs WHERE id = $1`, claim.ID).Scan(&p); err != nil {
			t.Fatal(err)
		}
		a, _ := p["output_attempts"].(map[string]any)
		return a
	}
	for _, st := range v.Journal.Steps {
		cfg := map[string]any{"id": st.Request.ColID, "type": "output"}
		for k, x := range st.Request.ColExtra {
			cfg[k] = x
		}
		col := column{cfg: cfg, ID: st.Request.ColID, Type: "output"}
		lead := map[string]any{"company": "Acme"}
		if st.Request.RowID != nil {
			lead["__row_id"] = *st.Request.RowID
		}
		if st.Name == "dangling_claim_requires_review" { // a claim whose process died before the receipt
			digest, err := contractHash(rs.outputContract(lead, col, st.Request.LeadID))
			if err != nil {
				t.Fatal(err)
			}
			if err := rs.withJobPayload(ctx, func(a map[string]any) (bool, error) {
				a[outputCellKey(lead, col, st.Request.LeadID)] = map[string]any{"contract_hash": digest, "state": "dispatch_claimed"}
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		res, err := rs.executeOutputJournaled(ctx, lead, col, 0, st.Request.LeadID)
		if st.Result.Error != "" {
			if !errors.Is(err, errOutputCrash) {
				t.Fatalf("%s: want %s, got %v", st.Name, st.Result.Message, err)
			}
		} else {
			if err != nil {
				t.Fatalf("%s: %v", st.Name, err)
			}
			got := receiptJSON(res)
			if !reflect.DeepEqual(got, st.Result.Value) {
				t.Errorf("%s: receipt %v, python %v", st.Name, got, st.Result.Value)
			}
		}
		if !reflect.DeepEqual(calls, st.Calls) {
			t.Errorf("%s: sends %v, python %v", st.Name, calls, st.Calls)
		}
		if a := attempts(); !reflect.DeepEqual(a, st.Attempts) {
			ga, _ := json.Marshal(a)
			wa, _ := json.Marshal(st.Attempts)
			t.Errorf("%s: journal\n got  %s\n want %s", st.Name, ga, wa)
		}
	}
	// A lost lease writes nothing.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'completed' WHERE id = $1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	err = rs.withJobPayload(ctx, func(map[string]any) (bool, error) { return true, nil })
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("lost lease: %v", err)
	}
}
