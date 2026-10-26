package playbooks

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func keyOrder(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func render(t *testing.T, v any) string {
	t.Helper()
	rec := httptest.NewRecorder()
	httpx.WriteJSON(rec, 200, v)
	return rec.Body.String()
}

func str(s string) *string { return &s }
func tp(t time.Time) *time.Time { return &t }

func TestModelJSONKeyOrderMatchesToAPI(t *testing.T) {
	vf := loadVectors(t)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	pb := Playbook{ID: "p1", Name: "a<b>&", Description: "", PromptTemplate: "tmpl tmpl tmpl",
		Steps: []Step{{Key: "k", Name: "n", PromptTemplate: "ppppppppppp", OutputFormat: "text"}}, OutputFormat: "text",
		MaxSteps: 4, CellBudgetUSD: 0.1, Version: 1, Enabled: true, CreatedAt: ts, UpdatedAt: ts}
	got := render(t, pb)
	if k := keyOrder(t, got); strings.Join(k, ",") != strings.Join(vf.ToAPIKeys["playbook"], ",") {
		t.Fatalf("playbook keys %v want %v", k, vf.ToAPIKeys["playbook"])
	}
	want := `{"id":"p1","name":"a<b>&","description":"","prompt_template":"tmpl tmpl tmpl","steps":[{"key":"k","name":"n","prompt_template":"ppppppppppp","output_format":"text"}],"output_format":"text","max_steps":4,"cell_budget_usd":0.1,"version":1,"enabled":true,"schedule_audience_id":null,"schedule_interval_minutes":null,"next_run_at":null,"created_at":"2026-01-02T03:04:05.123456","updated_at":"2026-01-02T03:04:05.123456"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// floats render like Python repr; datetimes drop zero microseconds; nil steps is [].
	pb2 := Playbook{ID: "p2", CellBudgetUSD: 5, ScheduleAudienceID: str("aud"), ScheduleIntervalMinutes: func() *int { i := 30; return &i }(),
		NextRunAt: tp(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), CreatedAt: ts, UpdatedAt: ts}
	g2 := render(t, pb2)
	for _, frag := range []string{`"steps":[]`, `"cell_budget_usd":5.0`, `"schedule_audience_id":"aud"`, `"schedule_interval_minutes":30`, `"next_run_at":"2026-01-02T03:04:05"`} {
		if !strings.Contains(g2, frag) {
			t.Errorf("missing %s in %s", frag, g2)
		}
	}

	run := Run{ID: "r1", PlaybookID: "p1", AudienceID: "a1", Status: "failed", PromptVersion: 2, MaxMembers: 100, Attempted: 3, Succeeded: 1, Failed: 2,
		Error: str("boom"), StartedAt: tp(ts), CreatedAt: ts}
	g := render(t, run)
	if k := keyOrder(t, g); strings.Join(k, ",") != strings.Join(vf.ToAPIKeys["run"], ",") {
		t.Fatalf("run keys %v want %v", k, vf.ToAPIKeys["run"])
	}
	if !strings.Contains(g, `"finished_at":null`) || strings.Contains(g, "steps") || strings.Contains(g, "snapshot") {
		t.Fatalf("run body %s", g)
	}

	res := Result{ID: 7, RunID: "r1", LeadID: 9, Status: "success", Value: "v", Metadata: json.RawMessage(`{"b":1,"a":{"z":2.50}}`), Attempts: 1, CreatedAt: ts, UpdatedAt: ts}
	g = render(t, res)
	if k := keyOrder(t, g); strings.Join(k, ",") != strings.Join(vf.ToAPIKeys["result"], ",") {
		t.Fatalf("result keys %v want %v", k, vf.ToAPIKeys["result"])
	}
	if !strings.Contains(g, `"metadata":{"b":1,"a":{"z":2.5}}`) {
		t.Fatalf("metadata not re-emitted in order/normalized: %s", g)
	}
	// metadata falsy -> {} (Python: result_metadata or {}).
	for _, m := range []string{``, `null`, `{}`, `[]`, `0`, `""`, `false`} {
		res.Metadata = json.RawMessage(m)
		if g := render(t, res); !strings.Contains(g, `"metadata":{}`) {
			t.Errorf("metadata %q -> %s", m, g)
		}
	}
}

func TestPagesJSONKeyOrder(t *testing.T) {
	off := 0
	nc := "cur"
	got := render(t, RunPage{Runs: []Run{}, Limit: 50, Offset: &off, HasMore: false})
	if got != `{"runs":[],"limit":50,"offset":0,"has_more":false,"next_cursor":null}` {
		t.Fatal(got)
	}
	got = render(t, ResultPage{Results: nil, Limit: 100, HasMore: true, NextCursor: &nc})
	if got != `{"results":[],"limit":100,"offset":null,"has_more":true,"next_cursor":"cur"}` {
		t.Fatal(got)
	}
}
