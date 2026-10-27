//go:build integration

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

type preVector struct {
	Name     string                    `json:"name"`
	Message  string                    `json:"message"`
	Approved bool                      `json:"approved"`
	Priors   map[string]map[string]any `json:"priors"`
	Settings struct {
		Poller bool `json:"INTENT_POLLER_ENABLED"`
		PG     bool `json:"PG_LEAD_STORE"`
	} `json:"settings"`
	Accounts map[string][]string `json:"accounts"`
	Frames   []struct {
		T        string  `json:"t"`
		Line     string  `json:"line"`
		Role     string  `json:"role"`
		Content  string  `json:"content"`
		ToolData *string `json:"tool_data"`
	} `json:"frames"`
	Approvals []struct {
		ToolCall map[string]any `json:"tool_call"`
	} `json:"approvals"`
}

// preDelegated are the corpus cases delegated to the worker when the engine has no People (leadpeople):
// explicit people research and verification. tools_people_integration_test.go replays them with People.
var preDelegated = map[string]bool{"explicit_people_card": true, "verify": true}

// preGoFixes are the Python defects fixed in Go: recorded text -> Go text.
var preGoFixes = map[string][2]string{
	"team_domain": {"**stripe.com e.com sales leaders**", "**find stripe.com sales leaders**"},
	"explicit_people": {"**people currently working on Stripe's ipe's partnerships team.**",
		"**Find people currently working on Stripe's partnerships team.**"},
}

func TestNativePreRouteMatchesPython(t *testing.T) {
	var f struct {
		Seq   []string    `json:"uuid_hex_sequence"`
		Cases []preVector `json:"cases"`
	}
	readVectors(t, "native_preroute.json", &f)
	e := persistSetup(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = e.owner.Exec(ctx, `DELETE FROM workbook_rows WHERE workspace_id = $1`, e.ws)
		_, _ = e.owner.Exec(ctx, `DELETE FROM workbooks WHERE workspace_id = $1`, e.ws)
		_, _ = e.owner.Exec(ctx, `DELETE FROM chat_tool_approvals WHERE workspace_id = $1`, e.ws)
	})
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			e.reset(t)
			_, _ = e.owner.Exec(ctx, `DELETE FROM workbook_rows WHERE workspace_id = $1`, e.ws)
			_, _ = e.owner.Exec(ctx, `DELETE FROM workbooks WHERE workspace_id = $1`, e.ws)
			_, _ = e.owner.Exec(ctx, `DELETE FROM chat_tool_approvals WHERE workspace_id = $1`, e.ws)
			for name, res := range c.Priors {
				td, _ := json.Marshal(map[string]any{"name": name, "result": res})
				s := string(td)
				if err := tenancy.InWorkspace(ctx, e.pool, e.ws, func(tx pgx.Tx) error {
					return chathistory.AddMessage(ctx, tx, e.conv, "tool", name+" result", &s, e.ws, i32(7))
				}); err != nil {
					t.Fatal(err)
				}
			}
			for wb, ids := range c.Accounts {
				if _, err := e.owner.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1, 'Accounts', $2)`, wb, e.ws); err != nil {
					t.Fatal(err)
				}
				for i, id := range ids {
					if _, err := e.owner.Exec(ctx, `INSERT INTO workbook_rows (workbook_id, position, data, workspace_id) VALUES ($1, $2, $3, $4)`,
						wb, i, `{"account_id": "`+id+`"}`, e.ws); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := len(e.messages(t))
			n := 0
			tn := e.turn()
			tn.Engine = &Engine{IntentPollerEnabled: c.Settings.Poller, PGLeadStore: c.Settings.PG,
				NewID: func() string { n++; return f.Seq[n-1] }}
			rec := newRec()
			w, _ := ssestream.New(rec, nil)
			handled, delegate, err := tn.nativePreRoute(ctx, w, c.Message, c.Approved)
			if err != nil {
				t.Fatal(err)
			}
			if preDelegated[c.Name] {
				if !delegate || handled || rec.String() != "" {
					t.Fatalf("want delegation, got handled=%v delegate=%v out=%q", handled, delegate, rec.String())
				}
				return
			}
			if delegate {
				t.Fatal("unexpected delegation")
			}
			var want strings.Builder
			var persisted []string
			for _, fr := range c.Frames {
				switch fr.T {
				case "sse":
					want.WriteString(fr.Line)
				case "persist":
					persisted = append(persisted, fr.Role+"|"+fr.Content)
				}
			}
			if len(c.Frames) == 1 && c.Frames[0].T == "none" {
				if handled || rec.String() != "" {
					t.Fatalf("want none, got handled=%v out=%q", handled, rec.String())
				}
				return
			}
			if !handled {
				t.Fatal("not handled")
			}
			// Go ids: the real conversation id and approval ids stand in for the recorded conv-1 / appr-N.
			var approvals []struct{ ID, Call string }
			rows, err := e.owner.Query(ctx, `SELECT id, tool_call_json FROM chat_tool_approvals WHERE workspace_id = $1 ORDER BY created_at_epoch, id`, e.ws)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var a struct{ ID, Call string }
				if err := rows.Scan(&a.ID, &a.Call); err != nil {
					t.Fatal(err)
				}
				approvals = append(approvals, a)
			}
			rows.Close()
			norm := func(s string) string {
				s = strings.ReplaceAll(s, e.conv, "conv-1")
				for i, a := range approvals {
					s = strings.ReplaceAll(s, a.ID, "appr-"+string(rune('1'+i)))
				}
				return s
			}
			wantS := want.String()
			if fx, ok := preGoFixes[c.Name]; ok {
				wantS = strings.ReplaceAll(wantS, fx[0], fx[1])
				for i := range persisted {
					persisted[i] = strings.ReplaceAll(persisted[i], fx[0], fx[1])
				}
			}
			if c.Name == "accounts_unicode" { // Go keeps the "São Paulo" geography Python's [a-z] dropped
				if got := rec.String(); !strings.Contains(got, "geography S\\u00e3o Paulo") {
					t.Fatalf("output %q", got)
				}
				return
			}
			if got := norm(rec.String()); got != wantS {
				t.Fatalf("sse:\n got %q\nwant %q", got, wantS)
			}
			ms := e.messages(t)[before:]
			if len(ms) != len(persisted) {
				t.Fatalf("persisted %d rows, want %d", len(ms), len(persisted))
			}
			for i, m := range ms {
				if got := m.Role + "|" + m.Content; got != persisted[i] {
					t.Errorf("row %d: %q, want %q", i, got, persisted[i])
				}
			}
			if len(approvals) != len(c.Approvals) {
				t.Fatalf("approvals %d, want %d", len(approvals), len(c.Approvals))
			}
			for i, a := range c.Approvals {
				wb, err := canonjson.Marshal(normalizeJSON(t, a.ToolCall), true)
				if err != nil {
					t.Fatal(err)
				}
				if got := norm(approvals[i].Call); got != string(wb) {
					t.Errorf("approval %d:\n got %s\nwant %s", i, got, wb)
				}
			}
		})
	}
}

// normalizeJSON re-decodes v so numbers are canonjson shapes.
func normalizeJSON(t *testing.T, v any) any {
	b, _ := json.Marshal(v)
	out, err := canonjson.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNativePreRouteWithoutPeopleAndGoBranches(t *testing.T) {
	e := persistSetup(t)
	// Without People the explicit people research branch cannot be answered: one fixed error frame and [DONE].
	tn := e.turn()
	tn.Engine = &Engine{}
	handled, out, err := runPre(t, tn, "Find named people currently working on Stripe's partnerships team.", false)
	if err == nil || !handled || out != "data: {\"error\": \"AI provider stream interrupted\"}\n\ndata: [DONE]\n\n" {
		t.Fatalf("people branch without People: handled=%v err=%v out=%q", handled, err, out)
	}
	// Before the response started, the same fault is reported as ErrPreRouteOpen with nothing written.
	tn.OpenFault = func() bool { return true }
	handled, out, err = runPre(t, tn, "Find named people currently working on Stripe's partnerships team.", false)
	if !errors.Is(err, ErrPreRouteOpen) || handled || out != "" {
		t.Fatalf("open fault: handled=%v err=%v out=%q", handled, err, out)
	}
	// A Go branch and "no branch".
	tn = e.turn()
	tn.Engine = &Engine{}
	handled, out, err = runPre(t, tn, "Stripe partnerships team", false)
	if err != nil || !handled || !strings.Contains(out, "intent_clarification") {
		t.Fatalf("clarification: handled=%v err=%v out=%q", handled, err, out)
	}
	handled, out, err = runPre(t, tn, "hello", false)
	if err != nil || handled || out != "" {
		t.Fatalf("none: handled=%v err=%v out=%q", handled, err, out)
	}
	// A turn without an engine has no pre-route.
	handled, out, err = runPre(t, e.turn(), "Stripe partnerships team", false)
	if err != nil || handled || out != "" {
		t.Fatalf("no engine: handled=%v err=%v out=%q", handled, err, out)
	}
}
