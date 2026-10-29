package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// bootstrapFake records BootstrapSchedule calls and can fail them.
type bootstrapFake struct {
	fakeAutomation
	bootErr error
	inTx    bool
}

func (f *bootstrapFake) BootstrapSchedule(_ context.Context, _, id string) error {
	f.Bootstrap = append(f.Bootstrap, id)
	return f.bootErr
}

const storedSignal = `{"id": "auto-1", "workspace_id": "ws-alpha", "name": "Hot", "enabled": true, "trigger_type": "on_signal", "trigger_config": {}}`
const storedSchedule = `{"id": "auto-2", "workspace_id": "ws-alpha", "name": "Daily", "enabled": true, "trigger_type": "on_schedule", "trigger_config": {"interval": "daily"}}`

func autoTool(t *testing.T, d Deps) WriteTool {
	t.Helper()
	ws := automationWriteTools(d)
	if len(ws) != 1 || ws[0].Name != "create_automation" {
		t.Fatalf("tools = %+v", ws)
	}
	return ws[0]
}

func autoDeps(p AutomationPort, cfg Config) Deps {
	cfg.AutomationsEnabled = true
	if cfg.AutomationsMaxRulesPerWS == 0 {
		cfg.AutomationsMaxRulesPerWS = 50
	}
	return Deps{InWorkspace: nilTxRunner, Cfg: cfg, Automation: p}
}

func applyAuto(t *testing.T, d Deps, args Args) (string, error) {
	t.Helper()
	uid := int32(7)
	return autoTool(t, d).Apply(context.Background(), nil, Ctx{WorkspaceID: "ws-alpha", UserID: &uid, Capabilities: AllCaps}, args)
}

// captureRule records the RuleInput passed to the port.
type captureRule struct {
	bootstrapFake
	got   RuleInput
	valid RuleInput
}

func (f *captureRule) ValidateRule(_ context.Context, _ pgx.Tx, _ string, r RuleInput) *HTTPError {
	f.valid = r
	return f.Reject
}
func (f *captureRule) CreateTrigger(_ context.Context, _ pgx.Tx, _ string, r RuleInput) (json.RawMessage, error) {
	f.got = r
	return f.Stored, f.Err
}

func TestCreateAutomationMatchesFixture(t *testing.T) {
	_, want := execFixture(t, "writes__create_automation_on_signal.json")
	f := &captureRule{}
	f.Stored = json.RawMessage(`{"id": "00000000-0000-4000-8000-000000000065", "workspace_id": "ws-alpha", "name": "Hot signal hook", "enabled": true, "trigger_type": "on_signal", "trigger_config": {}, "condition": "", "actions": [{"type": "webhook", "config": {"url": "https://hooks.example.com/x", "api_key": "FAKE_NESTED_SECRET_VALUE"}}], "scope_workbook_ids": [], "stop_on_error": false, "max_spend_usd_per_day": null, "max_actions_per_day": null, "next_run_at": null, "schedule_anchor": null, "last_fired_at": null, "created_by": "1", "created_at": "<volatile>", "updated_at": "<volatile>"}`)
	got, err := applyAuto(t, autoDeps(f, Config{}), Args{
		"name": "Hot signal hook", "trigger_type": "on_signal", "condition": "",
		"actions": []any{map[string]any{"type": "webhook", "config": map[string]any{"url": "https://hooks.example.com/x", "api_key": "FAKE_NESTED_SECRET_VALUE"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Python defaults reach the port; created_by is the token's user.
	r := f.got
	if string(r.TriggerConfig) != `{}` || r.Condition != "" || len(r.ScopeWorkbookIDs) != 0 || !r.Enabled || r.StopOnError ||
		r.MaxActionsPerDay != nil || r.MaxSpendUSDPerDay != nil || r.CreatedBy == nil || *r.CreatedBy != 7 ||
		r.TriggerType == nil || *r.TriggerType != "on_signal" {
		t.Fatalf("rule = %+v", r)
	}
}

func TestCreateAutomationPassesExplicitFields(t *testing.T) {
	f := &captureRule{}
	f.Stored = json.RawMessage(storedSchedule)
	_, err := applyAuto(t, autoDeps(f, Config{}), Args{
		"name": "  Daily sweep ", "trigger_type": "on_schedule", "trigger_config": map[string]any{"interval": "daily"},
		"enabled": false, "stop_on_error": true, "max_spend_usd_per_day": 1.5, "max_actions_per_day": float64(20),
		"scope_workbook_ids": []any{"wb-alpha-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := f.got
	if r.Name != "Daily sweep" || string(r.TriggerConfig) != `{"interval":"daily"}` || r.Enabled || !r.StopOnError ||
		*r.MaxSpendUSDPerDay != 1.5 || *r.MaxActionsPerDay != 20 || len(r.ScopeWorkbookIDs) != 1 {
		t.Fatalf("rule = %+v", r)
	}
	if f.valid.Name != "Daily sweep" {
		t.Fatal("ValidateRule must see the same input")
	}
}

func TestCreateAutomationDisabledGate(t *testing.T) {
	_, want := execFixture(t, "automations_off__automations_off_create_automation.json")
	g := autoTool(t, Deps{}).Gate
	if reason := g(Config{}); reason != "automations disabled" || errorText(reason) != want {
		t.Fatalf("gate = %q", reason)
	}
	if g(Config{AutomationsEnabled: true}) != "" {
		t.Fatal("enabled config must pass the gate")
	}
}

func TestCreateAutomationNeedsAdmin(t *testing.T) {
	w := autoTool(t, Deps{})
	if w.Capability != CapAutomationsWrite || strings.Join(w.Roles, ",") != "admin" {
		t.Fatalf("cap %q roles %v", w.Capability, w.Roles)
	}
}

func TestCreateAutomationValidationErrorShape(t *testing.T) {
	_, want := execFixture(t, "writes__create_automation_invalid_trigger_type.json")
	f := &captureRule{}
	f.Reject = &HTTPError{Status: 422, Detail: "invalid trigger_type 'bogus'"}
	got, err := applyAuto(t, autoDeps(f, Config{}), Args{"name": "x", "trigger_type": "bogus"})
	if err != nil || got != want {
		t.Fatalf("got %q err %v want %q", got, err, want)
	}
	if f.got.Name != "" {
		t.Fatal("CreateTrigger must not run after a validation failure")
	}
	if msg, isErr := resultError(got); !isErr || msg == "" {
		t.Fatal("result must be an error result so the pipeline rolls back")
	}
}

func TestCreateAutomationMissingVsEmptyTriggerType(t *testing.T) {
	f := &captureRule{}
	d := autoDeps(f, Config{})
	f.Stored = json.RawMessage(storedSignal)
	if _, err := applyAuto(t, d, Args{"name": "x"}); err != nil {
		t.Fatal(err)
	}
	if f.valid.TriggerType != nil {
		t.Fatal("missing trigger_type must reach the port as nil")
	}
	if _, err := applyAuto(t, d, Args{"name": "x", "trigger_type": ""}); err != nil {
		t.Fatal(err)
	}
	if f.valid.TriggerType == nil || *f.valid.TriggerType != "" {
		t.Fatal("empty trigger_type must reach the port as a pointer to the empty string")
	}
	if _, err := applyAuto(t, d, Args{"name": "x", "trigger_type": nil}); err != nil {
		t.Fatal(err)
	}
	if f.valid.TriggerType != nil {
		t.Fatal("null trigger_type must reach the port as nil")
	}
}

func TestCreateAutomationNameRequired(t *testing.T) {
	_, want := execFixture(t, "writes__create_automation_name_required.json")
	f := &captureRule{}
	for _, name := range []any{"  ", "", nil} {
		got, err := applyAuto(t, autoDeps(f, Config{}), Args{"name": name, "trigger_type": "on_signal"})
		if err != nil || got != want {
			t.Fatalf("%v: got %q err %v", name, got, err)
		}
	}
	if got, _ := applyAuto(t, autoDeps(f, Config{}), Args{"trigger_type": "on_signal"}); got != want {
		t.Fatal(got)
	}
}

func TestCreateAutomationMaxRules(t *testing.T) {
	_, want := execFixture(t, "rules_cap__rules_cap_reached.json")
	f := &captureRule{}
	f.Rules = 1
	got, err := applyAuto(t, autoDeps(f, Config{AutomationsMaxRulesPerWS: 1}), Args{"name": "two", "trigger_type": "on_signal"})
	if err != nil || got != want {
		t.Fatalf("got %q err %v", got, err)
	}
	if f.got.Name != "" {
		t.Fatal("no insert at the cap")
	}
	// above the cap too, and below it works.
	f.Rules = 5
	if got, _ := applyAuto(t, autoDeps(f, Config{AutomationsMaxRulesPerWS: 1}), Args{"name": "two"}); got != want {
		t.Fatal(got)
	}
	f.Rules = 0
	f.Stored = json.RawMessage(storedSignal)
	_, wantFirst := execFixture(t, "rules_cap__rules_cap_first.json")
	got, err = applyAuto(t, autoDeps(f, Config{AutomationsMaxRulesPerWS: 1}), Args{"name": "one", "trigger_type": "on_signal"})
	if err != nil || strings.Contains(got, "max rules") || !strings.HasPrefix(wantFirst, `{"status": "created"`) {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestCreateAutomationRejectsWrongTypes(t *testing.T) {
	cases := []struct {
		args Args
		msg  string
	}{
		{Args{"name": 5.0}, "name must be a string"},
		{Args{"name": "x", "trigger_type": 3.0}, "trigger_type must be a string"},
		{Args{"name": "x", "trigger_config": "daily"}, "trigger_config must be an object"},
		{Args{"name": "x", "actions": "webhook"}, "actions must be a list of at most 50 objects"},
		{Args{"name": "x", "actions": []any{"webhook"}}, "actions must be a list of at most 50 objects"},
		{Args{"name": "x", "actions": make([]any, 51)}, "actions must be a list of at most 50 objects"},
		{Args{"name": "x", "condition": 1.0}, "condition must be a string"},
		{Args{"name": "x", "scope_workbook_ids": "wb"}, "scope_workbook_ids must be an array of strings"},
		{Args{"name": "x", "enabled": "yes"}, "enabled must be a boolean"},
		{Args{"name": "x", "stop_on_error": 1.0}, "stop_on_error must be a boolean"},
		{Args{"name": "x", "max_actions_per_day": 1.5}, "max_actions_per_day must be an integer"},
		{Args{"name": "x", "max_actions_per_day": "5"}, "max_actions_per_day must be an integer"},
		{Args{"name": "x", "max_spend_usd_per_day": "1"}, "max_spend_usd_per_day must be a number"},
		{Args{"name": "x", "max_spend_usd_per_day": true}, "max_spend_usd_per_day must be a number"},
	}
	for _, c := range cases {
		f := &captureRule{}
		got, err := applyAuto(t, autoDeps(f, Config{}), c.args)
		if err != nil {
			t.Fatal(c.msg, err)
		}
		if got != errorText(c.msg) {
			t.Errorf("%v: got %s want %s", c.args, got, c.msg)
		}
		if f.got.Name != "" || f.valid.Name != "" {
			t.Errorf("%s: port reached", c.msg)
		}
	}
	// exactly 50 objects is fine.
	acts := make([]any, 50)
	for i := range acts {
		acts[i] = map[string]any{"type": "webhook"}
	}
	f := &captureRule{}
	f.Stored = json.RawMessage(storedSignal)
	if got, _ := applyAuto(t, autoDeps(f, Config{}), Args{"name": "x", "actions": acts}); strings.Contains(got, "error") {
		t.Fatal(got)
	}
}

func TestCreateAutomationScheduleBootstrapAfterCommit(t *testing.T) {
	f := &bootstrapFake{}
	f.Stored = json.RawMessage(storedSchedule)
	d := autoDeps(f, Config{})
	res, err := applyAuto(t, d, Args{"name": "Daily", "trigger_type": "on_schedule", "trigger_config": map[string]any{"interval": "daily"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Bootstrap) != 0 {
		t.Fatal("bootstrap must not run inside Apply (pre-commit)")
	}
	s := NewServer(d, NewRegistry(nil, automationWriteTools(d)))
	s.postCommit(context.Background(), Ctx{WorkspaceID: "ws-alpha"}, "create_automation", res)
	if len(f.Bootstrap) != 1 || f.Bootstrap[0] != "auto-2" {
		t.Fatalf("bootstrap = %v", f.Bootstrap)
	}
	// the result is the shape the hook reads.
	var shape struct {
		ID   string `json:"automation_id"`
		Auto struct {
			TT string `json:"trigger_type"`
			En bool   `json:"enabled"`
		} `json:"automation"`
	}
	if err := json.Unmarshal([]byte(res), &shape); err != nil || shape.ID != "auto-2" || shape.Auto.TT != "on_schedule" || !shape.Auto.En {
		t.Fatalf("shape = %+v err %v", shape, err)
	}
	// not for on_signal, not for disabled schedules.
	f.Bootstrap = nil
	f.Stored = json.RawMessage(storedSignal)
	res, _ = applyAuto(t, d, Args{"name": "x", "trigger_type": "on_signal"})
	s.postCommit(context.Background(), Ctx{WorkspaceID: "ws-alpha"}, "create_automation", res)
	f.Stored = json.RawMessage(strings.Replace(storedSchedule, `"enabled": true`, `"enabled": false`, 1))
	res, _ = applyAuto(t, d, Args{"name": "x", "trigger_type": "on_schedule", "enabled": false})
	s.postCommit(context.Background(), Ctx{WorkspaceID: "ws-alpha"}, "create_automation", res)
	if len(f.Bootstrap) != 0 {
		t.Fatalf("bootstrap = %v", f.Bootstrap)
	}
}

func TestCreateAutomationBootstrapFailureDoesNotFailCreate(t *testing.T) {
	f := &bootstrapFake{bootErr: errors.New("boom secret-ish detail")}
	f.Stored = json.RawMessage(storedSchedule)
	d := autoDeps(f, Config{})
	res, err := applyAuto(t, d, Args{"name": "Daily", "trigger_type": "on_schedule"})
	if err != nil || !strings.HasPrefix(res, `{"status": "created"`) {
		t.Fatalf("res %q err %v", res, err)
	}
	slog.SetDefault(slog.New(slog.DiscardHandler))
	s := NewServer(d, NewRegistry(nil, automationWriteTools(d)))
	s.postCommit(context.Background(), Ctx{WorkspaceID: "ws-alpha"}, "create_automation", res) // must not panic
	if len(f.Bootstrap) != 1 {
		t.Fatal("bootstrap not attempted")
	}
}

func TestCreateAutomationUnavailableWithoutPort(t *testing.T) {
	d := Deps{InWorkspace: nilTxRunner, Cfg: Config{AutomationsEnabled: true, AutomationsMaxRulesPerWS: 50}}
	_, err := applyAuto(t, d, Args{"name": "x", "trigger_type": "on_signal"})
	if err == nil || !IsUnavailable(err) || safeErrorText(err) != "not available in this build" {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateAutomationPortErrorMasked(t *testing.T) {
	f := &captureRule{}
	f.Err = errors.New(`pq: relation "triggers" secret`)
	_, err := applyAuto(t, autoDeps(f, Config{}), Args{"name": "x"})
	if err == nil || safeErrorText(err) != "internal error" {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateAutomationAuditRedactsNestedSecrets(t *testing.T) {
	args := Args{"name": "x", "actions": []any{map[string]any{"type": "webhook", "config": map[string]any{"url": "https://h.example/x", "api_key": "FAKE_NESTED_SECRET_VALUE"}}}}
	b, _ := json.Marshal(RedactArguments(args))
	if strings.Contains(string(b), "FAKE_NESTED_SECRET_VALUE") || !strings.Contains(string(b), "https://h.example/x") {
		t.Fatalf("audit args = %s", b)
	}
}
