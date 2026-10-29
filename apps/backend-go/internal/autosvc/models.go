// Package autosvc is the automation rule service: rule validation, CRUD, run
// history and manual-run enqueue (routers/automations.py), plus the to_api
// JSON shapes. Every tenant statement runs inside tenancy.InWorkspace.
package autosvc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Iso renders t like Python's datetime.isoformat() for a UTC-aware value:
// microseconds only when non-zero, then "+00:00".
func Iso(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "+00:00"
}

func isoPtr(v pgtype.Timestamptz) any {
	if !v.Valid {
		return nil
	}
	return Iso(v.Time)
}

// ParseJSON decodes a stored JSON column into ordered values (objects are
// *wbcrud.Obj, numbers json.Number canonicalised like a Python round trip).
// Empty input and JSON null are nil.
func ParseJSON(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	wrapped := make([]byte, 0, len(raw)+6)
	wrapped = append(wrapped, `{"v":`...)
	wrapped = append(wrapped, raw...)
	wrapped = append(wrapped, '}')
	o, err := wbcrud.ParseObjectCanon(wrapped)
	if err != nil {
		return nil, err
	}
	v, _ := o.Get("v")
	return v, nil
}

// truthy is Python truthiness for decoded JSON values.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		return err != nil || f != 0
	case []any:
		return len(t) > 0
	case *wbcrud.Obj:
		return t.Len() > 0
	}
	return true
}

// orDefault is `value or default`.
func orDefault(v, def any) any {
	if truthy(v) {
		return v
	}
	return def
}

func emptyObj() any { return wbcrud.NewObj() }

// pyRound4 is round(x, 4) as a float.
func pyRound4(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return f
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 4, 64), 64)
	if err != nil {
		return f
	}
	return r
}

// Trigger is one automation rule (Trigger.to_api).
type Trigger struct {
	ID, WorkspaceID, Name string
	Enabled               bool
	TriggerType           string
	TriggerConfig         any // ordered JSON value
	Condition             string
	Actions               any
	ScopeWorkbookIDs      any
	StopOnError           bool
	MaxSpendUSDPerDay     *float64
	MaxActionsPerDay      *int32
	NextRunAt             *time.Time
	ScheduleAnchor        *time.Time
	LastFiredAt           *time.Time
	CreatedBy             *string
	CreatedAt, UpdatedAt  *time.Time
}

func tp(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}

func tsIso(t *time.Time) any {
	if t == nil {
		return nil
	}
	return Iso(*t)
}

func toTrigger(r gen.Trigger) (Trigger, error) {
	t := Trigger{ID: r.ID, WorkspaceID: r.WorkspaceID, Name: r.Name, Enabled: r.Enabled, TriggerType: r.TriggerType,
		Condition: r.Condition.String, StopOnError: r.StopOnError,
		NextRunAt: tp(r.NextRunAt), ScheduleAnchor: tp(r.ScheduleAnchor), LastFiredAt: tp(r.LastFiredAt),
		CreatedAt: tp(r.CreatedAt), UpdatedAt: tp(r.UpdatedAt)}
	var err error
	if t.TriggerConfig, err = ParseJSON(r.TriggerConfig); err != nil {
		return t, fmt.Errorf("autosvc: stored trigger_config of %s: %w", r.ID, err)
	}
	if t.Actions, err = ParseJSON(r.Actions); err != nil {
		return t, fmt.Errorf("autosvc: stored actions of %s: %w", r.ID, err)
	}
	if t.ScopeWorkbookIDs, err = ParseJSON(r.ScopeWorkbookIds); err != nil {
		return t, fmt.Errorf("autosvc: stored scope_workbook_ids of %s: %w", r.ID, err)
	}
	if r.MaxSpendUsdPerDay.Valid {
		f := r.MaxSpendUsdPerDay.Float64
		t.MaxSpendUSDPerDay = &f
	}
	if r.MaxActionsPerDay.Valid {
		n := r.MaxActionsPerDay.Int32
		t.MaxActionsPerDay = &n
	}
	if r.CreatedBy.Valid {
		s := r.CreatedBy.String
		t.CreatedBy = &s
	}
	return t, nil
}

func floatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return jsonFloat(*f)
}

func jsonFloat(f float64) json.Number {
	s, err := canonjson.FloatRepr(f)
	if err != nil {
		return json.Number("null")
	}
	return json.Number(s)
}

// Obj renders the rule as an ordered object in to_api key order.
func (t Trigger) Obj() *wbcrud.Obj {
	o := wbcrud.NewObj()
	o.Set("id", t.ID)
	o.Set("workspace_id", t.WorkspaceID)
	o.Set("name", t.Name)
	o.Set("enabled", t.Enabled)
	o.Set("trigger_type", t.TriggerType)
	o.Set("trigger_config", orDefault(t.TriggerConfig, emptyObj()))
	o.Set("condition", t.Condition)
	o.Set("actions", orDefault(t.Actions, []any{}))
	o.Set("scope_workbook_ids", orDefault(t.ScopeWorkbookIDs, []any{}))
	o.Set("stop_on_error", t.StopOnError)
	o.Set("max_spend_usd_per_day", floatOrNil(t.MaxSpendUSDPerDay))
	if t.MaxActionsPerDay == nil {
		o.Set("max_actions_per_day", nil)
	} else {
		o.Set("max_actions_per_day", int64(*t.MaxActionsPerDay))
	}
	o.Set("next_run_at", tsIso(t.NextRunAt))
	o.Set("schedule_anchor", tsIso(t.ScheduleAnchor))
	o.Set("last_fired_at", tsIso(t.LastFiredAt))
	if t.CreatedBy == nil {
		o.Set("created_by", nil)
	} else {
		o.Set("created_by", *t.CreatedBy)
	}
	o.Set("created_at", tsIso(t.CreatedAt))
	o.Set("updated_at", tsIso(t.UpdatedAt))
	return o
}

// MarshalJSON is Trigger.to_api.
func (t Trigger) MarshalJSON() ([]byte, error) { return t.Obj().MarshalJSON() }

// TriggerDetail is get_trigger: to_api plus recent_runs (latest 10).
type TriggerDetail struct {
	Trigger
	RecentRuns []Run
}

// MarshalJSON appends recent_runs after the to_api keys.
func (d TriggerDetail) MarshalJSON() ([]byte, error) {
	o := d.Trigger.Obj()
	runs := make([]any, len(d.RecentRuns))
	for i, r := range d.RecentRuns {
		runs[i] = r.Obj()
	}
	o.Set("recent_runs", runs)
	return o.MarshalJSON()
}

// Run is one trigger_runs row (TriggerRun.to_api).
type Run struct {
	ID, TriggerID                                                   string
	TriggerNameSnapshot                                             *string
	JobID                                                           *int32
	Status, FireSource, FireKey                                     *string
	MatchedRows, ActionsAttempted, ActionsSucceeded, ActionsSkipped int32
	ActionsFailed                                                   int32
	TotalChargedUSD                                                 float64
	Error                                                           *string
	StartedAt, FinishedAt                                           *time.Time
}

func textp(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

func strOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func toRun(r gen.TriggerRun) Run {
	x := Run{ID: r.ID, TriggerID: r.TriggerID, TriggerNameSnapshot: textp(r.TriggerNameSnapshot),
		Status: textp(r.Status), FireSource: textp(r.FireSource), FireKey: textp(r.FireKey),
		MatchedRows: r.MatchedRows.Int32, ActionsAttempted: r.ActionsAttempted.Int32,
		ActionsSucceeded: r.ActionsSucceeded.Int32, ActionsSkipped: r.ActionsSkipped.Int32,
		ActionsFailed: r.ActionsFailed.Int32, TotalChargedUSD: r.TotalChargedUsd.Float64,
		Error: textp(r.Error), StartedAt: tp(r.StartedAt), FinishedAt: tp(r.FinishedAt)}
	if r.JobID.Valid {
		j := r.JobID.Int32
		x.JobID = &j
	}
	return x
}

// Obj renders the run in to_api key order.
func (r Run) Obj() *wbcrud.Obj {
	o := wbcrud.NewObj()
	o.Set("id", r.ID)
	o.Set("trigger_id", r.TriggerID)
	o.Set("trigger_name_snapshot", strOrNil(r.TriggerNameSnapshot))
	if r.JobID == nil {
		o.Set("job_id", nil)
	} else {
		o.Set("job_id", int64(*r.JobID))
	}
	o.Set("status", strOrNil(r.Status))
	o.Set("fire_source", strOrNil(r.FireSource))
	o.Set("fire_key", strOrNil(r.FireKey))
	o.Set("matched_rows", int64(r.MatchedRows))
	o.Set("actions_attempted", int64(r.ActionsAttempted))
	o.Set("actions_succeeded", int64(r.ActionsSucceeded))
	o.Set("actions_skipped", int64(r.ActionsSkipped))
	o.Set("actions_failed", int64(r.ActionsFailed))
	o.Set("total_charged_usd", jsonFloat(pyRound4(r.TotalChargedUSD)))
	o.Set("error", strOrNil(r.Error))
	o.Set("started_at", tsIso(r.StartedAt))
	o.Set("finished_at", tsIso(r.FinishedAt))
	return o
}

// MarshalJSON is TriggerRun.to_api.
func (r Run) MarshalJSON() ([]byte, error) { return r.Obj().MarshalJSON() }

// RunDetail is get_run: to_api plus action_results.
type RunDetail struct {
	Run
	ActionResults []ActionResult
}

// MarshalJSON appends action_results after the to_api keys.
func (d RunDetail) MarshalJSON() ([]byte, error) {
	o := d.Run.Obj()
	rs := make([]any, len(d.ActionResults))
	for i, r := range d.ActionResults {
		rs[i] = r.Obj()
	}
	o.Set("action_results", rs)
	return o.MarshalJSON()
}

// ActionResult is one trigger_action_results row (TriggerActionResult.to_api).
type ActionResult struct {
	ID                                       int32
	RunID, TriggerID, WorkbookID, RowID      string
	LeadID                                   *int32
	ActionIndex                              int32
	ActionType, IdempotencyKey               string
	Status, SkipReason, ResultSummary, Error *string
	ChargedUSD                               float64
	CreatedAt                                *time.Time
}

func toResult(r gen.TriggerActionResult) ActionResult {
	x := ActionResult{ID: r.ID, RunID: r.RunID, TriggerID: r.TriggerID, WorkbookID: r.WorkbookID, RowID: r.RowID,
		ActionIndex: r.ActionIndex, ActionType: r.ActionType, IdempotencyKey: r.IdempotencyKey,
		Status: textp(r.Status), SkipReason: textp(r.SkipReason), ResultSummary: textp(r.ResultSummary),
		Error: textp(r.Error), ChargedUSD: r.ChargedUsd.Float64, CreatedAt: tp(r.CreatedAt)}
	if r.LeadID.Valid {
		l := r.LeadID.Int32
		x.LeadID = &l
	}
	return x
}

// Obj renders the result in to_api key order.
func (r ActionResult) Obj() *wbcrud.Obj {
	o := wbcrud.NewObj()
	o.Set("id", int64(r.ID))
	o.Set("run_id", r.RunID)
	o.Set("trigger_id", r.TriggerID)
	o.Set("workbook_id", r.WorkbookID)
	o.Set("row_id", r.RowID)
	if r.LeadID == nil {
		o.Set("lead_id", nil)
	} else {
		o.Set("lead_id", int64(*r.LeadID))
	}
	o.Set("action_index", int64(r.ActionIndex))
	o.Set("action_type", r.ActionType)
	o.Set("idempotency_key", r.IdempotencyKey)
	o.Set("status", strOrNil(r.Status))
	o.Set("skip_reason", strOrNil(r.SkipReason))
	o.Set("charged_usd", jsonFloat(pyRound4(r.ChargedUSD)))
	o.Set("result_summary", strOrNil(r.ResultSummary))
	o.Set("error", strOrNil(r.Error))
	o.Set("created_at", tsIso(r.CreatedAt))
	return o
}

// MarshalJSON is TriggerActionResult.to_api.
func (r ActionResult) MarshalJSON() ([]byte, error) { return r.Obj().MarshalJSON() }

// Created is the create result the MCP adapter returns:
// {"automation": <to_api>, "automation_id": <id>}.
type Created struct {
	Automation Trigger
}

// AutomationID is the new rule's id.
func (c Created) AutomationID() string { return c.Automation.ID }

// MarshalJSON renders {"automation":..., "automation_id":...}.
func (c Created) MarshalJSON() ([]byte, error) {
	o := wbcrud.NewObj()
	o.Set("automation", c.Automation.Obj())
	o.Set("automation_id", c.Automation.ID)
	return o.MarshalJSON()
}

// maskedValue replaces secret-bearing webhook config for callers without
// automations.manage (S-L3).
const maskedValue = "***"

// Masked returns a copy of t for a caller who may read rules but not manage them:
// webhook action config.headers values become "***", and the url keeps only scheme
// and host (the path and query often carry the secret, e.g. a Slack hook). Key names,
// header_secret_ref and everything else stay visible. t is not modified.
func (t Trigger) Masked() Trigger {
	acts, ok := t.Actions.([]any)
	if !ok {
		return t
	}
	out := make([]any, len(acts))
	for i, a := range acts {
		out[i] = maskAction(a)
	}
	t.Actions = out
	return t
}

func maskAction(a any) any {
	act, ok := a.(*wbcrud.Obj)
	if !ok {
		return a
	}
	if typ, _ := act.Get("type"); typ != "webhook" {
		return a
	}
	cv, _ := act.Get("config")
	cfg, ok := cv.(*wbcrud.Obj)
	if !ok {
		return a
	}
	nc := wbcrud.NewObj()
	for _, k := range cfg.Keys() {
		v, _ := cfg.Get(k)
		switch k {
		case "url":
			if s, ok := v.(string); ok {
				v = maskURL(s)
			}
		case "headers":
			if h, ok := v.(*wbcrud.Obj); ok {
				nh := wbcrud.NewObj()
				for _, hk := range h.Keys() {
					nh.Set(hk, maskedValue)
				}
				v = nh
			} else if v != nil {
				v = maskedValue
			}
		}
		nc.Set(k, v)
	}
	na := wbcrud.NewObj()
	for _, k := range act.Keys() {
		v, _ := act.Get(k)
		if k == "config" {
			v = nc
		}
		na.Set(k, v)
	}
	return na
}

// maskURL keeps scheme://authority (userinfo dropped) and hides any path, query or fragment.
func maskURL(u string) string {
	sep := strings.Index(u, "://")
	if sep < 0 {
		return maskedValue
	}
	rest := u[sep+3:]
	end := len(rest)
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		end = j
	}
	auth := rest[:end]
	if at := strings.LastIndexByte(auth, '@'); at >= 0 {
		auth = auth[at+1:]
	}
	if end == len(rest) {
		return u[:sep+3] + auth
	}
	return u[:sep+3] + auth + "/" + maskedValue
}
