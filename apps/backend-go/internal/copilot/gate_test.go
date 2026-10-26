//go:build integration

package copilot

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type approvalRow struct{ ID, UserID, Status, CallJSON string }

func approvalsFor(t *testing.T, pool *pgxpool.Pool, ws string) []approvalRow {
	t.Helper()
	var out []approvalRow
	err := tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id, user_id, status, tool_call_json FROM chat_tool_approvals WHERE workspace_id=$1 ORDER BY created_at_epoch, id`, ws)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r approvalRow
			if err := rows.Scan(&r.ID, &r.UserID, &r.Status, &r.CallJSON); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func gateTurn(t *testing.T, f *fakePy, ws string) *Turn {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	tn := newTurn(f, nativeConst(`{"must":"not run"}`))
	tn.WS = ws
	tn.Auth.WorkspaceID = ws
	tn.Pool = pool
	t.Cleanup(func() {
		o, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
		if err == nil {
			_, _ = o.Exec(context.Background(), `DELETE FROM chat_tool_approvals WHERE workspace_id=$1`, ws)
			o.Close()
		}
	})
	return tn
}

const dangerousCalls = `[{"id":"call_d1","type":"function","function":{"name":"update_lead_status","arguments":"{\"lead_id\": 4, \"status\": \"contacted\"}"}}]`

func TestDangerousCallNeverExecuted(t *testing.T) {
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + dangerousCalls + `,"cursor":0}`})
	tn := gateTurn(t, f, "ws-t13-never")
	native := 0
	tn.Native = func(context.Context, string, map[string]any, string, *int32) (string, bool, error) {
		native++
		return "", true, nil
	}
	body, err := runTurn(t, tn, userMsgs())
	if err != nil {
		t.Fatal(err)
	}
	if f.toolCount() != 0 || native != 0 {
		t.Fatalf("dangerous tool executed: python %d native %d", f.toolCount(), native)
	}
	if f.llmCount() != 1 {
		t.Fatalf("the turn must end for approval, llm calls %d", f.llmCount())
	}
	if !strings.HasSuffix(body, frame(`{"awaiting_confirmation": true}`)+done) {
		t.Fatalf("body %q", body)
	}
}

func TestUnknownToolTreatedDangerous(t *testing.T) {
	calls := `[{"id":"h1","type":"function","function":{"name":"drop_all_tables","arguments":"{}"}}]`
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`})
	tn := gateTurn(t, f, "ws-t13-unknown")
	body, _ := runTurn(t, tn, userMsgs())
	if f.toolCount() != 0 {
		t.Fatal("unknown tool executed")
	}
	if !strings.Contains(body, `"level": "high", "label": "Unrecognized write tool"}}`) {
		t.Fatalf("body %q", body)
	}
	if rows := approvalsFor(t, tn.Pool, "ws-t13-unknown"); len(rows) != 1 {
		t.Fatalf("approvals %v", rows)
	}
}

func TestConfirmationEventKeyOrder(t *testing.T) {
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + dangerousCalls + `,"cursor":0}`})
	f.descFn = func(b map[string]any) (int, any) { return 200, map[string]any{"description": "Changes → x"} }
	tn := gateTurn(t, f, "ws-t13-order")
	body, _ := runTurn(t, tn, userMsgs())
	rows := approvalsFor(t, tn.Pool, "ws-t13-order")
	if len(rows) != 1 {
		t.Fatalf("approvals %v", rows)
	}
	id := rows[0].ID
	want := frame(`{"confirmation_required": {"confirmation_id": "`+id+`", "tool_call": {"id": "`+id+`", "type": "function", "function": {"name": "update_lead_status", "arguments": "{\"lead_id\": 4, \"status\": \"contacted\"}"}}, "name": "update_lead_status", "args": {"lead_id": 4, "status": "contacted"}, "description": "Changes → x", "level": "medium", "label": "📝 Update Lead Status"}}`) +
		frame(`{"awaiting_confirmation": true}`) + done
	if body != want {
		t.Fatalf("got  %q\nwant %q", body, want)
	}
	if len(f.describe) != 1 || f.describe[0]["workspace_id"] != "ws-t13-order" || f.describe[0]["name"] != "update_lead_status" {
		t.Fatalf("describe %v", f.describe)
	}
}

func TestApprovalRowCreatedPerDangerousCall(t *testing.T) {
	calls := `[{"id":"s1","type":"function","function":{"name":"get_lead_stats","arguments":""}},
	 {"id":"d1","type":"function","function":{"name":"update_lead_status","arguments":"{\"lead_id\": 1, \"status\": \"new\"}"}},
	 {"id":"d2","type":"function","function":{"name":"enrich_lead","arguments":""}}]`
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`})
	tn := gateTurn(t, f, "ws-t13-per")
	tn.Native = nativeConst(`{"total": 1}`)
	body, _ := runTurn(t, tn, userMsgs())
	rows := approvalsFor(t, tn.Pool, "ws-t13-per")
	if len(rows) != 2 {
		t.Fatalf("approvals %v", rows)
	}
	for _, r := range rows {
		if r.Status != "pending" || r.UserID != "7" {
			t.Fatalf("row %+v", r)
		}
		var call map[string]any
		if err := json.Unmarshal([]byte(r.CallJSON), &call); err != nil || call["function"] == nil {
			t.Fatalf("stored call %q", r.CallJSON)
		}
	}
	// the stored call carries the "{}" fix for the empty arguments
	if !strings.Contains(rows[0].CallJSON+rows[1].CallJSON, `"arguments":"{}"`) {
		t.Fatalf("rows %+v", rows)
	}
	// event order: the safe call's tool_call/tool_result, then one confirmation per dangerous call, then the gate
	iCall := strings.Index(body, `{"tool_call": {"name": "get_lead_stats"`)
	iRes := strings.Index(body, `{"tool_result"`)
	iC1 := strings.Index(body, `{"confirmation_required"`)
	iC2 := strings.LastIndex(body, `{"confirmation_required"`)
	iAw := strings.Index(body, `{"awaiting_confirmation"`)
	if !(0 <= iCall && iCall < iRes && iRes < iC1 && iC1 < iC2 && iC2 < iAw) {
		t.Fatalf("order %d %d %d %d %d in %q", iCall, iRes, iC1, iC2, iAw, body)
	}
	if strings.Count(body, "[DONE]") != 1 {
		t.Fatalf("body %q", body)
	}
	if f.toolCount() != 0 {
		t.Fatal("python tool endpoint must not be called")
	}
}

func TestDescribeFailureFallsBackToLabelAndReason(t *testing.T) {
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + dangerousCalls + `,"cursor":0}`})
	f.descFn = func(map[string]any) (int, any) { return 500, map[string]any{"detail": "describe failed"} }
	tn := gateTurn(t, f, "ws-t13-desc")
	body, err := runTurn(t, tn, userMsgs())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"description": "📝 Update Lead Status\nChanges the pipeline status of a lead."`) {
		t.Fatalf("body %q", body)
	}
}

func TestFramesReplayDangerousEqualsLegacyBytes(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	for _, name := range scenarioNames(t) {
		if !strings.Contains(name, "dangerous") && !strings.Contains(name, "unknown") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			got, want, _ := replayScenario(t, name, pool)
			if norm(got) != norm(want) {
				t.Fatalf("bytes differ\n got %q\nwant %q", got, want)
			}
		})
	}
}
