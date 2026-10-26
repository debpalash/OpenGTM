package autosvc

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// marshal renders like httpx.WriteJSON (no HTML escaping).
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

func parse(t *testing.T, raw string) any {
	t.Helper()
	v, err := ParseJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTriggerJSONKeyOrder(t *testing.T) {
	ts := time.Date(2026, 8, 29, 9, 30, 0, 123456000, time.UTC)
	whole := time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)
	spend := 12.5
	n := int32(3)
	by := "7"
	tr := Trigger{ID: "t1", WorkspaceID: "w", Name: "Ünï \"q\" <b>", Enabled: true, TriggerType: "on_schedule",
		TriggerConfig: parse(t, `{"interval": "daily", "z": 1.50, "a": [1, 2e2]}`), Condition: "{a} == 1",
		Actions:          parse(t, `[{"type": "webhook", "config": {"url": "https://h/x", "b": 1, "a": 2}}]`),
		ScopeWorkbookIDs: parse(t, `["wb1"]`), StopOnError: true, MaxSpendUSDPerDay: &spend, MaxActionsPerDay: &n,
		NextRunAt: &ts, ScheduleAnchor: &whole, CreatedBy: &by, CreatedAt: &whole, UpdatedAt: &ts}
	got := marshal(tr)
	want := `{"id":"t1","workspace_id":"w","name":"Ünï \"q\" <b>","enabled":true,"trigger_type":"on_schedule",` +
		`"trigger_config":{"interval":"daily","z":1.5,"a":[1,200.0]},"condition":"{a} == 1",` +
		`"actions":[{"type":"webhook","config":{"url":"https://h/x","b":1,"a":2}}],"scope_workbook_ids":["wb1"],` +
		`"stop_on_error":true,"max_spend_usd_per_day":12.5,"max_actions_per_day":3,` +
		`"next_run_at":"2026-08-29T09:30:00.123456+00:00","schedule_anchor":"2026-08-29T09:30:00+00:00",` +
		`"last_fired_at":null,"created_by":"7","created_at":"2026-08-29T09:30:00+00:00",` +
		`"updated_at":"2026-08-29T09:30:00.123456+00:00"}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestTriggerJSONFalsyDefaults(t *testing.T) {
	// `x or default`: a stored null/{}/[] renders the default.
	tr := Trigger{ID: "t", WorkspaceID: "w", Name: "n", TriggerType: "on_signal"}
	got := marshal(tr)
	want := `{"id":"t","workspace_id":"w","name":"n","enabled":false,"trigger_type":"on_signal","trigger_config":{},` +
		`"condition":"","actions":[],"scope_workbook_ids":[],"stop_on_error":false,"max_spend_usd_per_day":null,` +
		`"max_actions_per_day":null,"next_run_at":null,"schedule_anchor":null,"last_fired_at":null,"created_by":null,` +
		`"created_at":null,"updated_at":null}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestRunAndResultJSON(t *testing.T) {
	st := "completed"
	ts := time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)
	run := Run{ID: "r1", TriggerID: "t1", Status: &st, MatchedRows: 2, TotalChargedUSD: 0.123456, StartedAt: &ts}
	got := marshal(RunDetail{Run: run, ActionResults: []ActionResult{{ID: 4, RunID: "r1", TriggerID: "t1",
		WorkbookID: "wb", RowID: "9", ActionIndex: 1, ActionType: "webhook", IdempotencyKey: "k", ChargedUSD: 0.00005, CreatedAt: &ts}}})
	want := `{"id":"r1","trigger_id":"t1","trigger_name_snapshot":null,"job_id":null,"status":"completed","fire_source":null,` +
		`"fire_key":null,"matched_rows":2,"actions_attempted":0,"actions_succeeded":0,"actions_skipped":0,"actions_failed":0,` +
		`"total_charged_usd":0.1235,"error":null,"started_at":"2026-08-29T09:30:00+00:00","finished_at":null,` +
		`"action_results":[{"id":4,"run_id":"r1","trigger_id":"t1","workbook_id":"wb","row_id":"9","lead_id":null,` +
		`"action_index":1,"action_type":"webhook","idempotency_key":"k","status":null,"skip_reason":null,"charged_usd":0.0001,` +
		`"result_summary":null,"error":null,"created_at":"2026-08-29T09:30:00+00:00"}]}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCreatedShapeForMCP(t *testing.T) {
	c := Created{Automation: Trigger{ID: "t9", WorkspaceID: "w", Name: "n", Enabled: true, TriggerType: "on_schedule"}}
	b := marshal(c)
	o, err := wbcrud.ParseObject(b)
	if err != nil {
		t.Fatal(err)
	}
	if k := o.Keys(); len(k) != 2 || k[0] != "automation" || k[1] != "automation_id" {
		t.Fatalf("keys %v", k)
	}
	a, _ := o.Get("automation")
	if tt, _ := a.(*wbcrud.Obj).Get("trigger_type"); tt != "on_schedule" {
		t.Fatalf("trigger_type %v", tt)
	}
	if en, _ := a.(*wbcrud.Obj).Get("enabled"); en != true {
		t.Fatalf("enabled %v", en)
	}
}

func TestPyIntAndURLParse(t *testing.T) {
	for in, want := range map[string]struct {
		n  int64
		ok bool
	}{"12": {12, true}, " 12 ": {12, true}, "+5": {5, true}, "1_0": {10, true}, "abc": {0, false}, "": {0, false},
		"1__0": {0, false}, "_1": {0, false}, "1.0": {0, false}, "1e3": {0, false}, "--1": {0, false}} {
		n, ok := pyInt(in)
		if n != want.n || ok != want.ok {
			t.Errorf("pyInt(%q) = %d,%v", in, n, ok)
		}
	}
	p, ok := urlparse("  HTTPS://u:p@Host.Example.com:8080/x?y=1")
	if !ok || p.scheme != "https" || p.username != "u" || p.password != "p" || p.hostname != "host.example.com" {
		t.Fatalf("%+v %v", p, ok)
	}
	if _, ok := urlparse("http://[::1/x"); ok {
		t.Fatal("unbalanced bracket must fail like Python's ValueError")
	}
	if p, _ := urlparse("http://[::1]:80/x"); p.hostname != "::1" {
		t.Fatalf("%+v", p)
	}
}
