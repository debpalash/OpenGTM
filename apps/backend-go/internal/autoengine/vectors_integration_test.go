//go:build integration

package autoengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
)

type vOutcome struct {
	Status     string  `json:"status"`
	Summary    string  `json:"summary"`
	Error      *string `json:"error"`
	SkipReason *string `json:"skip_reason"`
	Raise      string  `json:"raise"`
}

type vRow struct {
	Data json.RawMessage `json:"data"`
	Enr  json.RawMessage `json:"enr"`
	Lead *int            `json:"lead"`
	WB   string          `json:"wb"`
}

type vPre struct {
	Row     int     `json:"row"`
	Action  int     `json:"action"`
	Status  string  `json:"status"`
	FireKey string  `json:"fire_key"`
	Type    string  `json:"type"`
	Skip    *string `json:"skip_reason"`
	Charged float64 `json:"charged"`
	WB      string  `json:"wb"`
}

type vSpec struct {
	Payload   map[string]json.RawMessage   `json:"payload"`
	Trigger   map[string]json.RawMessage   `json:"trigger"`
	Columns   json.RawMessage              `json:"columns"`
	ColumnsB  json.RawMessage              `json:"columns_b"`
	Rows      *[]vRow                      `json:"rows"`
	NoTrigger bool                         `json:"no_trigger"`
	Balance   *float64                     `json:"balance"`
	Pre       []vPre                       `json:"pre"`
	LedgerPre []string                     `json:"ledger_pre"`
	Outcomes  map[string]vOutcome          `json:"outcomes"`
	ExtraRuns []map[string]json.RawMessage `json:"extra_runs"`
	Settings  struct {
		Enabled   *bool    `json:"enabled"`
		MaxRows   *int     `json:"max_rows"`
		GlobalCap *float64 `json:"global_cap"`
	} `json:"settings"`
}

type vRun struct {
	Status     string  `json:"status"`
	FireSource string  `json:"fire_source"`
	FireKey    string  `json:"fire_key"`
	Name       *string `json:"trigger_name_snapshot"`
	Matched    int     `json:"matched_rows"`
	Attempted  int     `json:"actions_attempted"`
	Succeeded  int     `json:"actions_succeeded"`
	Skipped    int     `json:"actions_skipped"`
	Failed     int     `json:"actions_failed"`
	Charged    float64 `json:"total_charged_usd"`
	Finished   bool    `json:"finished"`
}

type vResult struct {
	Row         json.RawMessage `json:"row"`
	WorkbookID  string          `json:"workbook_id"`
	ActionIndex int             `json:"action_index"`
	ActionType  string          `json:"action_type"`
	LeadID      *int            `json:"lead_id"`
	Status      string          `json:"status"`
	SkipReason  *string         `json:"skip_reason"`
	Charged     float64         `json:"charged_usd"`
	Summary     *string         `json:"result_summary"`
	Error       *string         `json:"error"`
	Key         string          `json:"idempotency_key"`
	Pre         bool            `json:"pre_existing"`
}

type vCase struct {
	Name         string `json:"name"`
	Spec         vSpec  `json:"spec"`
	Runs         []vRun `json:"runs"`
	Results      []vResult
	Reservations []struct {
		TriggerID string  `json:"trigger_id"`
		Reserved  float64 `json:"reserved_usd"`
		Actions   int     `json:"reserved_actions"`
		Key       string  `json:"idempotency_key"`
		State     string  `json:"state"`
	} `json:"reservations"`
	Balance float64 `json:"balance_usd"`
	Ledger  []struct {
		Amount float64 `json:"amount_usd"`
		Reason string  `json:"reason"`
		RunID  string  `json:"run_id"`
	} `json:"ledger"`
	Activity []struct {
		WorkbookID string `json:"workbook_id"`
		Kind       string `json:"kind"`
		Message    string `json:"message"`
	} `json:"activity"`
	LastFired bool `json:"last_fired_set"`
	Calls     []struct {
		Row      int            `json:"row"`
		Type     string         `json:"action_type"`
		WB       string         `json:"workbook_id"`
		LeadID   *int           `json:"lead_id"`
		LeadData map[string]any `json:"lead_data"`
		Idem     string         `json:"idem"`
	} `json:"execute_calls"`
	Resched []map[string]string `json:"reschedule_calls"`
}

func normJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func ps(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestEvalMatchesPythonRowsForFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/run_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []vCase `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	prev := billing.ResearchVendorCost
	billing.ResearchVendorCost = func() bool { return true }
	defer func() { billing.ResearchVendorCost = prev }()
	if len(file.Vectors) < 70 {
		t.Fatalf("only %d vectors", len(file.Vectors))
	}
	e := newEnv(t)
	for _, vc := range file.Vectors {
		vc := vc
		t.Run(vc.Name, func(t *testing.T) { e.t = t; runVector(t, e, vc) })
	}
}

func runVector(t *testing.T, e *env, vc vCase) {
	e.clean()
	e.rowIDs = nil
	sp := vc.Spec
	cols := engCols
	if sp.Columns != nil {
		cols = string(sp.Columns)
	}
	colsB := string(engCols)
	colsB = `[{"id":"company","name":"Company","type":"lead_field","lead_field":"company"},{"id":"email","name":"Email","type":"lead_field","lead_field":"email"}]`
	if sp.ColumnsB != nil {
		colsB = string(sp.ColumnsB)
	}
	e.workbook(ws1, "wbA", cols)
	e.workbook(ws1, "wbB", colsB)
	e.workbook(ws2, "wbX", cols)
	rows := []vRow{
		{Data: json.RawMessage(`{"company":"Acme","email":"a@acme.com","score":"90"}`)},
		{Data: json.RawMessage(`{"company":"Beta","email":"","score":"20"}`)},
		{Data: json.RawMessage(`{"company":"Gamma","email":"g@gamma.com","score":"70"}`), Enr: json.RawMessage(`{"free":{"value":"g2@gamma.com","status":"complete"}}`), Lead: ptr(7)},
	}
	if sp.Rows != nil {
		rows = *sp.Rows
	}
	wbOf := map[string]string{"": "wbA", "A": "wbA", "B": "wbB", "X": "wbX"}
	for _, r := range rows {
		wb := wbOf[r.WB]
		ws := ws1
		if wb == "wbX" {
			ws = ws2
		}
		e.row(ws, wb, string(r.Data), string(r.Enr), r.Lead)
	}
	ordinal := map[string]int{}
	for i, id := range e.rowIDs {
		ordinal[strconv.Itoa(int(id))] = i
	}

	if !sp.NoTrigger {
		tr := trig{}
		for k, v := range sp.Trigger {
			switch k {
			case "actions":
				tr.Actions = string(v)
			case "condition":
				_ = json.Unmarshal(v, &tr.Condition)
			case "enabled":
				var b bool
				_ = json.Unmarshal(v, &b)
				tr.Enabled = &b
			case "scope_workbook_ids":
				tr.Scope = string(v)
			case "stop_on_error":
				_ = json.Unmarshal(v, &tr.StopOnError)
			case "max_spend_usd_per_day":
				var f float64
				_ = json.Unmarshal(v, &f)
				tr.MaxSpend = &f
			case "max_actions_per_day":
				var n int
				_ = json.Unmarshal(v, &n)
				tr.MaxActions = &n
			default:
				t.Fatalf("unknown trigger key %s", k)
			}
		}
		e.trigger(ws1, "trg-1", tr)
	}
	bal := 10.0
	if sp.Balance != nil {
		bal = *sp.Balance
	}
	e.credits(ws1, bal)
	idemOf := func(r vPre) string {
		wb := r.WB
		if wb == "" {
			wb = "wbA"
		}
		return fmtKey(ws1, "trg-1", wb, e.rowIDs[r.Row], r.Action, r.FireKey)
	}
	for _, p := range sp.Pre {
		typ := p.Type
		if typ == "" {
			typ = "webhook"
		}
		wb := p.WB
		if wb == "" {
			wb = "wbA"
		}
		e.inWS(ws1, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, `INSERT INTO trigger_action_results (workspace_id, run_id, trigger_id, workbook_id, row_id, action_index,
				action_type, idempotency_key, status, skip_reason, charged_usd) VALUES ($1,'run-pre','trg-1',$2,$3,$4,$5,$6,$7,$8,$9)`,
				ws1, wb, strconv.Itoa(int(e.rowIDs[p.Row])), p.Action, typ, idemOf(p), p.Status, p.Skip, p.Charged)
			return err
		})
	}
	for _, rid := range sp.LedgerPre {
		e.inWS(ws1, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, `INSERT INTO credit_ledger_entries (workspace_id, amount_usd, reason, run_id, idempotency_key)
				VALUES ($1,-0.05,'pre',$2,$3)`, ws1, normKeyIDs(rid, e.rowIDs), "run:"+normKeyIDs(rid, e.rowIDs))
			return err
		})
	}

	// the fake executor: outcomes keyed "<row ordinal>.<action index>" or "*"
	var calls []map[string]any
	e.d.Execute = func(ctx context.Context, ws string, action map[string]any, wb, rowID string, leadID *int64,
		lead map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		parts := strings.Split(idem, ":")
		key := fmt.Sprintf("%d.%s", ordinal[rowID], parts[5])
		var lid any
		if leadID != nil {
			lid = *leadID
		}
		calls = append(calls, map[string]any{"row": ordinal[rowID], "action_type": action["type"], "workbook_id": wb, "lead_id": lid,
			"lead_data": lead, "idem": idem})
		o, ok := sp.Outcomes[key]
		if !ok {
			if o, ok = sp.Outcomes["*"]; !ok {
				o = vOutcome{Status: "success", Summary: "ok"}
			}
		}
		if o.Raise != "" {
			panic(o.Raise)
		}
		r := autoactions.Result{Status: o.Status, Summary: o.Summary}
		if o.Error != nil {
			r.Error = *o.Error
		}
		if o.SkipReason != nil {
			r.SkipReason = *o.SkipReason
		}
		return r
	}
	e.d.Enabled = sp.Settings.Enabled == nil || *sp.Settings.Enabled
	e.d.MaxRowsPerEval = 500
	if sp.Settings.MaxRows != nil {
		e.d.MaxRowsPerEval = *sp.Settings.MaxRows
	}
	e.d.GlobalDailyUSD = 0
	if sp.Settings.GlobalCap != nil {
		e.d.GlobalDailyUSD = *sp.Settings.GlobalCap
	}

	var claimIDs []int64
	runs := []map[string]json.RawMessage{sp.Payload}
	runs = append(runs, sp.ExtraRuns...)
	for _, rp := range runs {
		pl := map[string]any{}
		for k, v := range rp {
			var x any
			if err := json.Unmarshal(v, &x); err != nil {
				t.Fatal(err)
			}
			if k == "targets" {
				pl[k] = e.convertTargets(t, v, wbOf)
				continue
			}
			if x == nil {
				continue // a null workspace_id/trigger_id means absent
			}
			pl[k] = x
		}
		if _, has := rp["trigger_id"]; !has {
			pl["trigger_id"] = "trg-1"
		}
		if _, has := rp["workspace_id"]; !has {
			pl["workspace_id"] = ws1
		}
		c := e.claimRaw(pl)
		claimIDs = append(claimIDs, c.ID)
		if err := Handle(e.ctx, e.d, c); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	// ── compare ──
	gotRuns := e.runs(ws1)
	if len(gotRuns) != len(vc.Runs) {
		t.Fatalf("runs: got %d, want %d (%+v)", len(gotRuns), len(vc.Runs), gotRuns)
	}
	for i, w := range vc.Runs {
		g := gotRuns[i]
		if strings.HasPrefix(w.FireKey, "job:") {
			w.FireKey = fmt.Sprintf("job:%d", claimIDs[i])
		}
		if g.Status != w.Status || g.FireSource != w.FireSource || g.FireKey != w.FireKey || ps(g.Name) != ps(w.Name) ||
			g.Matched != w.Matched || g.Attempted != w.Attempted || g.OK != w.Succeeded || g.Skipped != w.Skipped ||
			g.Failed != w.Failed || !near(g.Charged, w.Charged) || g.Finished != w.Finished {
			t.Fatalf("run %d: got %+v want %+v", i, g, w)
		}
	}
	gotRes := e.results(ws1)
	if len(gotRes) != len(vc.Results) {
		t.Fatalf("results: got %d, want %d: %+v", len(gotRes), len(vc.Results), gotRes)
	}
	for i, w := range vc.Results {
		g := gotRes[i]
		wantRow := string(w.Row)
		wantRow = strings.Trim(wantRow, `"`)
		gotKey := normKey(g.Key, ordinal)
		wantKey := normKeyVec(w.Key, vc, wantRow)
		gotOrd := fmt.Sprint(ordinal[g.Row])
		if _, ok := ordinal[g.Row]; !ok {
			gotOrd = g.Row
		}
		if gotOrd != wantRow || g.WB != w.WorkbookID || g.Idx != w.ActionIndex || g.Type != w.ActionType || g.Status != w.Status ||
			ps(g.Skip) != ps(w.SkipReason) || !near(g.Charged, w.Charged) || ps(g.Summary) != ps(w.Summary) || ps(g.Error) != ps(w.Error) ||
			gotKey != wantKey || (g.RunID == "run-pre") != w.Pre || !reflect.DeepEqual(g.Lead, w.LeadID) {
			t.Fatalf("result %d:\n got %+v (ord %s key %s)\nwant %+v (key %s)", i, g, gotOrd, gotKey, w, wantKey)
		}
	}
	// reservations
	type rv struct {
		Trig, Key, State string
		USD              float64
		N                int
	}
	var gotRv []rv
	e.inWS(ws1, func(tx pgx.Tx) error {
		rs, err := tx.Query(e.ctx, `SELECT trigger_id, idempotency_key, state, reserved_usd, reserved_actions FROM trigger_cap_reservations ORDER BY id`)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r rv
			if err := rs.Scan(&r.Trig, &r.Key, &r.State, &r.USD, &r.N); err != nil {
				return err
			}
			r.Key = normKey(r.Key, ordinal)
			gotRv = append(gotRv, r)
		}
		return rs.Err()
	})
	if len(gotRv) != len(vc.Reservations) {
		t.Fatalf("reservations: got %+v want %+v", gotRv, vc.Reservations)
	}
	for i, w := range vc.Reservations {
		g := gotRv[i]
		if g.Trig != w.TriggerID || g.State != w.State || !near(g.USD, w.Reserved) || g.N != w.Actions || g.Key != normKeyVec(w.Key, vc, "") {
			t.Fatalf("reservation %d: got %+v want %+v", i, g, w)
		}
	}
	if b := e.balance(ws1); !near(b, vc.Balance) {
		t.Fatalf("balance %v want %v", b, vc.Balance)
	}
	// ledger (the Go key is scoped to the workspace; compare reason, amount, run id)
	type lg struct {
		Amount        float64
		Reason, RunID string
	}
	var gotLg, wantLg []lg
	e.inWS(ws1, func(tx pgx.Tx) error {
		rs, err := tx.Query(e.ctx, `SELECT amount_usd, reason, run_id FROM credit_ledger_entries WHERE reason <> 'pre' ORDER BY id`)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var l lg
			if err := rs.Scan(&l.Amount, &l.Reason, &l.RunID); err != nil {
				return err
			}
			l.RunID = normKey(l.RunID, ordinal)
			gotLg = append(gotLg, l)
		}
		return rs.Err()
	})
	for _, l := range vc.Ledger {
		if l.Reason == "pre" {
			continue
		}
		wantLg = append(wantLg, lg{l.Amount, l.Reason, normKeyVec(l.RunID, vc, "")})
	}
	if len(gotLg) != len(wantLg) {
		t.Fatalf("ledger got %+v want %+v", gotLg, wantLg)
	}
	for i := range gotLg {
		if !near(gotLg[i].Amount, wantLg[i].Amount) || gotLg[i].Reason != wantLg[i].Reason || gotLg[i].RunID != wantLg[i].RunID {
			t.Fatalf("ledger %d got %+v want %+v", i, gotLg[i], wantLg[i])
		}
	}
	// activity
	type act struct{ WB, Kind, Msg string }
	var gotAct, wantAct []act
	e.inWS(ws1, func(tx pgx.Tx) error {
		rs, err := tx.Query(e.ctx, `SELECT workbook_id, kind, message FROM workbook_activity`)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var a act
			if err := rs.Scan(&a.WB, &a.Kind, &a.Msg); err != nil {
				return err
			}
			gotAct = append(gotAct, a)
		}
		return rs.Err()
	})
	for _, a := range vc.Activity {
		wantAct = append(wantAct, act{a.WorkbookID, a.Kind, a.Message})
	}
	sortActs := func(x []act) {
		sort.Slice(x, func(i, j int) bool {
			if x[i].WB != x[j].WB {
				return x[i].WB < x[j].WB
			}
			return x[i].Msg < x[j].Msg
		})
	}
	sortActs(gotAct)
	sortActs(wantAct)
	if !reflect.DeepEqual(gotAct, wantAct) {
		t.Fatalf("activity got %+v want %+v", gotAct, wantAct)
	}
	// last_fired_at
	var fired bool
	e.inWS(ws1, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT COALESCE(bool_or(last_fired_at IS NOT NULL), false) FROM triggers WHERE id='trg-1'`).Scan(&fired)
	})
	if fired != vc.LastFired {
		t.Fatalf("last_fired_set %v want %v", fired, vc.LastFired)
	}
	// execute calls
	if len(calls) != len(vc.Calls) {
		t.Fatalf("execute calls: got %d want %d", len(calls), len(vc.Calls))
	}
	for i, w := range vc.Calls {
		g := calls[i]
		if g["row"] != w.Row || g["action_type"] != w.Type || g["workbook_id"] != w.WB || normKey(g["idem"].(string), ordinal) != normKeyVec(w.Idem, vc, "") {
			t.Fatalf("call %d: got %v want %+v", i, g, w)
		}
		var wl any
		if w.LeadID != nil {
			wl = int64(*w.LeadID)
		}
		if !reflect.DeepEqual(g["lead_id"], wl) {
			t.Fatalf("call %d lead_id %v want %v", i, g["lead_id"], wl)
		}
		want := map[string]any{}
		for k, v := range w.LeadData {
			want[k] = v
		}
		if w.LeadID == nil {
			want["id"] = float64(e.rowIDs[w.Row]) // id falls back to the row id
		}
		gl := normJSON(t, g["lead_data"]).(map[string]any)
		wl2 := normJSON(t, want).(map[string]any)
		if !reflect.DeepEqual(gl, wl2) {
			t.Fatalf("call %d lead_data got %v want %v", i, gl, wl2)
		}
	}
}

func (e *env) convertTargets(t *testing.T, raw json.RawMessage, wbOf map[string]string) []any {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	var out []any
	for _, it := range items {
		var pair []any
		if json.Unmarshal(it, &pair) == nil && len(pair) == 2 {
			wb := wbOf[pair[0].(string)]
			out = append(out, e.targetOf(wb, int(pair[1].(float64))))
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(it, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// normKey rewrites the numeric row id of an idempotency key (or a debit run id)
// to its seeding ordinal so Python's and Go's keys compare. Keys that are not
// trig keys pass through.
func normKey(k string, ordinal map[string]int) string {
	parts := strings.Split(k, ":")
	if len(parts) >= 7 && parts[0] == "trig" {
		if o, ok := ordinal[parts[4]]; ok {
			parts[4] = "r" + strconv.Itoa(o)
		}
	}
	return jobRE.ReplaceAllString(strings.Join(parts, ":"), "job:N")
}

var jobRE = regexp.MustCompile(`job:\d+$`)

// normKeyVec does the same for a recorded key, whose row id is Python's
// (1-based in seeding order).
func normKeyVec(k string, vc vCase, _ string) string {
	parts := strings.Split(k, ":")
	if len(parts) >= 7 && parts[0] == "trig" {
		if n, err := strconv.Atoi(parts[4]); err == nil {
			parts[4] = "r" + strconv.Itoa(n-1)
		}
	}
	return jobRE.ReplaceAllString(strings.Join(parts, ":"), "job:N")
}

// normKeyIDs rewrites a recorded ledger run id's Python row id into the real one.
func normKeyIDs(k string, rowIDs []int32) string {
	parts := strings.Split(k, ":")
	if len(parts) >= 7 && parts[0] == "trig" {
		if n, err := strconv.Atoi(parts[4]); err == nil && n >= 1 && n-1 < len(rowIDs) {
			parts[4] = strconv.Itoa(int(rowIDs[n-1]))
		}
	}
	return strings.Join(parts, ":")
}
