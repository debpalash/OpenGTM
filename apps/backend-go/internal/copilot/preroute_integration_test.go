//go:build integration

package copilot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

func (e *persistEnv) preTurn(f *preFake) *Turn {
	tn := e.turn()
	tn.PY = f.client()
	return tn
}

// withPersist appends persist frames before the final done frame of a recorded stream.
func withPersist(lines []string, persist ...string) []string {
	n := len(lines) - 1 // the recorded stream ends with done
	out := append([]string(nil), lines[:n]...)
	out = append(out, persist...)
	return append(out, lines[n])
}

func TestPeopleResearchBranchPersistsToolAndAssistantRows(t *testing.T) {
	e := persistSetup(t)
	lines, want := recordedLines(t, "pre_explicit_people")
	td := `{"name": "find_people_at_company", "result": {"ok": true, "n": 1}}`
	f := newPreFake(t, withPersist(lines,
		`{"t":"persist","seq":10,"role":"tool","content":"find_people_at_company result","tool_data":"{\"name\": \"find_people_at_company\", \"result\": {\"ok\": true, \"n\": 1}}"}`,
		`{"t":"persist","seq":11,"role":"assistant","content":"I found **1** — table","tool_data":null}`)...)
	handled, out, err := runPreWith(t, e.preTurn(f), "find people")
	if err != nil || !handled || out != want {
		t.Fatalf("handled=%v err=%v\n got %q", handled, err, out)
	}
	ms := e.messages(t)
	if len(ms) != 2 || ms[0].Role != "tool" || ms[0].ToolData == nil || *ms[0].ToolData != td ||
		ms[1].Role != "assistant" || ms[1].Content != "I found **1** — table" || ms[1].ToolData != nil {
		t.Fatalf("rows: %+v", ms)
	}
	if len(e.memories(t)) != 0 {
		t.Fatal("pre-route turns store no memory (Python parity)")
	}
}

func TestApprovalProposalBranchConfirmationBytes(t *testing.T) {
	e := persistSetup(t)
	lines, want := recordedLines(t, "pre_workbook_followup")
	// The approval the Python branch created in this tenant; Go must be able to consume it.
	var id string
	err := tenancy.InWorkspace(context.Background(), e.pool, e.ws, func(tx pgx.Tx) error {
		var err error
		id, err = chathistory.CreateApproval(context.Background(), tx, e.ws, i32(7), map[string]any{
			"id": "x", "type": "function", "function": map[string]any{"name": "create_people_workbook", "arguments": "{}"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f := newPreFake(t, withPersist(lines, `{"t":"persist","seq":9,"role":"assistant","content":"proposal","tool_data":null}`)...)
	handled, out, perr := runPreWith(t, e.preTurn(f), "Save those people into a workbook")
	if perr != nil || !handled || out != want {
		t.Fatalf("handled=%v err=%v\n got %q", handled, perr, out)
	}
	if ms := e.messages(t); len(ms) != 1 || ms[0].Content != "proposal" {
		t.Fatalf("rows: %+v", ms)
	}
	err = tenancy.InWorkspace(context.Background(), e.pool, e.ws, func(tx pgx.Tx) error {
		_, ok, err := chathistory.ConsumeApproval(context.Background(), tx, id, e.ws, i32(7), "approve", time.Hour, time.Now())
		if err == nil && !ok {
			err = errors.New("not consumed")
		}
		return err
	})
	if err != nil {
		t.Fatalf("approval not consumable by Go: %v", err)
	}
}

func TestDraftBlockedBranch(t *testing.T) {
	e := persistSetup(t)
	lines, want := recordedLines(t, "pre_draft_blocked")
	f := newPreFake(t, withPersist(lines, `{"t":"persist","seq":9,"role":"assistant","content":"blocked","tool_data":null}`)...)
	handled, out, err := runPreWith(t, e.preTurn(f), "Draft a short partnership email")
	if err != nil || !handled || out != want || !strings.Contains(out, `"send_performed": false`) {
		t.Fatalf("handled=%v err=%v\n got %q", handled, err, out)
	}
	if ms := e.messages(t); len(ms) != 1 || ms[0].Role != "assistant" {
		t.Fatalf("rows: %+v", ms)
	}
}

func TestPersistFrameFailureDoesNotBreakStream(t *testing.T) {
	e := persistSetup(t)
	f := newPreFake(t, `{"t":"persist","seq":1,"role":"assistant","content":"x","tool_data":null}`, `{"t":"sse","seq":2,"d":{"content":"ok"}}`, `{"t":"done"}`)
	tn := e.preTurn(f)
	tn.ConvID = "does-not-exist"
	handled, out, err := runPreWith(t, tn, "m")
	if err != nil || !handled || out != "data: {\"content\": \"ok\"}\n\ndata: [DONE]\n\n" {
		t.Fatalf("handled=%v out=%q err=%v", handled, out, err)
	}
}

func runPreWith(t *testing.T, tn *Turn, msg string) (bool, string, error) {
	t.Helper()
	return runPre(t, tn, msg, false)
}
