package autoactions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

type vecFile[T any] struct {
	Vectors []T `json:"vectors"`
}

func loadVectors[T any](t *testing.T, name string) []T {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var f vecFile[T]
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.Vectors
}

func parseCols(t *testing.T, raw json.RawMessage) []Column {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	cols, err := colgraph.ParseCols(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cols
}

func decodeAction(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// wireCost routes billing and the vendor catalog the way production does.
func wireCost(t *testing.T, research bool) {
	t.Helper()
	val := "0"
	if research {
		val = "1"
	}
	runplan.Wire(runplan.WireDeps{
		Index:      func() map[string]manifest.Entry { return nil },
		AppSetting: func(context.Context, string, string) (string, error) { return val, nil },
	})
	t.Cleanup(func() {
		vendorcatalog.SetDeclaredCost(nil)
		vendorcatalog.SetResearchFlag(nil)
		billing.SetCostCatalog(zeroCatalog{})
		runplan.ResetWiredForTest()
	})
}

type zeroCatalog struct{}

func (zeroCatalog) PlatformCost(string) (float64, bool) { return 0, false }

func TestProjectCostVectors(t *testing.T) {
	type vec struct {
		Name     string          `json:"name"`
		Action   json.RawMessage `json:"action"`
		Columns  json.RawMessage `json:"columns"`
		Research bool            `json:"research_vendor_cost"`
		Cost     float64         `json:"cost"`
	}
	vs := loadVectors[vec](t, "cost_vectors.json")
	if len(vs) < 50 {
		t.Fatalf("only %d vectors", len(vs))
	}
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			wireCost(t, v.Research)
			cols := parseCols(t, v.Columns)
			got := ProjectCost(decodeAction(t, v.Action), cols, DefaultWaterfallsFor(cols))
			if got != v.Cost {
				t.Fatalf("cost = %v, want %v", got, v.Cost)
			}
		})
	}
}

func TestProvidersByColumnFallbacks(t *testing.T) {
	cols := parseCols(t, json.RawMessage(`[
	  {"id":"w","waterfall":["x","y"],"provider":"p","target_field":"email"},
	  {"id":"p","provider":"only"},
	  {"id":"d","target_field":"email"},
	  {"id":"l","lead_field":"phone"},
	  {"id":"company_size"},
	  {"name":"noid","waterfall":["z"]},
	  {"id":"u","target_field":"nope"}]`))
	defaults := map[string][]string{"email": {"e1"}, "phone": {"ph1", "ph2"}, "company_size": {"cs"}}
	got := ProvidersByColumn([]any{"w", "p", "d", "l", "company_size", "u", "ghost", "w"}, cols, defaults)
	want := map[string][]string{"w": {"x", "y"}, "p": {"only"}, "d": {"e1"}, "l": {"ph1", "ph2"},
		"company_size": {"cs"}, "u": {}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

// ---- push_crm --------------------------------------------------------------

func TestPushCRMResultsMatchTranscripts(t *testing.T) {
	type crmCall struct {
		ColConfig struct {
			Destination       string          `json:"destination"`
			DestinationConfig json.RawMessage `json:"destination_config"`
		} `json:"col_config"`
		WorkbookID  string `json:"workbook_id"`
		LeadID      int64  `json:"lead_id"`
		WorkspaceID string `json:"workspace_id"`
	}
	type vec struct {
		Name          string          `json:"name"`
		Action        json.RawMessage `json:"action"`
		LeadID        *int64          `json:"lead_id"`
		Scripted      map[string]any  `json:"scripted"`
		ScriptedRaise *string         `json:"scripted_raises"`
		Result        *struct {
			Status     string  `json:"status"`
			Summary    string  `json:"summary"`
			Error      *string `json:"error"`
			SkipReason *string `json:"skip_reason"`
		} `json:"result"`
		CRMCall *crmCall `json:"crm_call"`
	}
	n := 0
	for _, v := range loadVectors[vec](t, "dispatch_vectors.json") {
		if v.CRMCall == nil || v.Result == nil {
			continue // push_crm_exception_propagates: Go never lets a crash escape (TestExecuteRecoversPanic)
		}
		n++
		t.Run(v.Name, func(t *testing.T) {
			var calls []crmCall
			d := Deps{Push: func(_ context.Context, ws string, cfg, lead map[string]any, wb string, leadID int64) crmpush.Result {
				calls = append(calls, crmCall{WorkbookID: wb, LeadID: leadID, WorkspaceID: ws})
				ok := false
				switch s := v.Scripted["success"].(type) {
				case bool:
					ok = s
				case float64:
					ok = s != 0
				}
				val, _ := v.Scripted["value"].(string)
				errS, _ := v.Scripted["error"].(string)
				return crmpush.Result{Success: ok, Value: val, Error: errS}
			}}
			act := decodeAction(t, v.Action)
			lid := v.LeadID
			got := Execute(context.Background(), d, "ws-1", act, "", "", lid, map[string]any{"x": 1}, nil, "idem")
			want := Result{Status: v.Result.Status, Summary: v.Result.Summary}
			if v.Result.Error != nil {
				want.Error = *v.Result.Error
			}
			if v.Result.SkipReason != nil {
				want.SkipReason = *v.Result.SkipReason
			}
			if got != want {
				t.Fatalf("got %+v want %+v", got, want)
			}
			if len(calls) != 1 || calls[0].WorkbookID != v.CRMCall.WorkbookID || calls[0].LeadID != v.CRMCall.LeadID || calls[0].WorkspaceID != v.CRMCall.WorkspaceID {
				t.Fatalf("crm call = %+v, want %+v", calls, v.CRMCall)
			}
		})
	}
	if n < 8 {
		t.Fatalf("only %d push_crm vectors ran", n)
	}
}

func TestPushCRMNotConnectedSkipReason(t *testing.T) {
	// Real crmpush.Push with a disconnected HubSpot: error "HubSpot not connected".
	d := Deps{CRM: crmpush.Deps{HubSpot: &recHS{}}}
	res := PushCRM(context.Background(), d, "ws-1", map[string]any{"type": "hubspot"}, map[string]any{"email": "a@x.io"}, nil, 7)
	if res.Status != "failed" || res.SkipReason != "not_connected" || !strings.Contains(strings.ToLower(res.Error), "not connected") {
		t.Fatalf("got %+v", res)
	}
	// An unrelated failure carries no skip reason.
	d = Deps{CRM: crmpush.Deps{HubSpot: &recHS{connected: true, push: crm.PushResult{Error: "boom"}}}}
	res = PushCRM(context.Background(), d, "ws-1", map[string]any{"type": "hubspot"}, map[string]any{"email": "a@x.io"}, nil, 7)
	if res.Status != "failed" || res.SkipReason != "" || res.Error != "boom" {
		t.Fatalf("got %+v", res)
	}
}

type recHS struct {
	mu        sync.Mutex
	connected bool
	push      crm.PushResult
	wsSeen    []string
}

func (f *recHS) rec(ws string) { f.mu.Lock(); f.wsSeen = append(f.wsSeen, ws); f.mu.Unlock() }
func (f *recHS) IsConnected(_ context.Context, ws string) bool {
	f.rec(ws)
	return f.connected
}
func (f *recHS) UpdateContactByID(_ context.Context, ws, _ string, _ crm.FieldMap) crm.UpdateResult {
	f.rec(ws)
	return crm.UpdateResult{}
}
func (f *recHS) PushLeadAsContact(_ context.Context, ws string, _ crm.Lead, _ crm.FieldMap) crm.PushResult {
	f.rec(ws)
	return f.push
}

func TestPushCRMNeverUsesOtherWorkspaceToken(t *testing.T) {
	hs := &recHS{connected: true, push: crm.PushResult{Success: true, Action: "created", HubspotID: "9"}}
	d := Deps{CRM: crmpush.Deps{HubSpot: hs}}
	for _, ws := range []string{"ws-a", "ws-b", "ws-a"} {
		res := Execute(context.Background(), d, ws, map[string]any{"type": "push_crm", "config": map[string]any{"type": "hubspot"}},
			"", "5", nil, map[string]any{"email": "a@x.io"}, nil, "")
		if res.Status != "success" {
			t.Fatalf("%s: %+v", ws, res)
		}
	}
	for _, ws := range hs.wsSeen {
		if ws != "ws-a" && ws != "ws-b" {
			t.Fatalf("unexpected workspace %q", ws)
		}
	}
	if len(hs.wsSeen) != 6 || hs.wsSeen[0] != "ws-a" || hs.wsSeen[2] != "ws-b" || hs.wsSeen[4] != "ws-a" {
		t.Fatalf("every CRM call must carry the action's own workspace, got %v", hs.wsSeen)
	}
}

func TestPushCRMKeepsOrderedFieldMap(t *testing.T) {
	var got crm.FieldMap
	hs := &fieldMapHS{recHS: recHS{connected: true, push: crm.PushResult{Success: true, Action: "created", HubspotID: "1"}}, fm: &got}
	cfg := colgraph.MakeCol("type", "hubspot", "field_map", colgraph.MakeCol("z_last", "Z", "a_first", "A", "m", "M"))
	res := PushCRM(context.Background(), Deps{CRM: crmpush.Deps{HubSpot: hs}}, "ws", map[string]any{"type": "hubspot", "field_map": cfg.Vals["field_map"]},
		map[string]any{"email": "a@x"}, nil, 1)
	if res.Status != "success" {
		t.Fatal(res)
	}
	var keys []string
	for _, p := range got {
		keys = append(keys, p.Key)
	}
	if !reflect.DeepEqual(keys, []string{"z_last", "a_first", "m"}) {
		t.Fatalf("field_map order lost: %v", keys)
	}
}

type fieldMapHS struct {
	recHS
	fm *crm.FieldMap
}

func (f *fieldMapHS) PushLeadAsContact(ctx context.Context, ws string, l crm.Lead, fm crm.FieldMap) crm.PushResult {
	*f.fm = fm
	return f.recHS.PushLeadAsContact(ctx, ws, l, fm)
}

func TestPushCRMCancelledIsFailedContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := Deps{Push: func(context.Context, string, map[string]any, map[string]any, string, int64) crmpush.Result {
		cancel()
		return crmpush.Result{Error: "HubSpot request failed"}
	}}
	res := PushCRM(ctx, d, "ws", map[string]any{}, map[string]any{}, nil, 1)
	if res.Status != "failed" || res.Error != "context canceled" || res.SkipReason != "" {
		t.Fatalf("got %+v", res)
	}
}

// ---- re_enrich -------------------------------------------------------------

type rrCall struct {
	WS, WB  string
	Cols    []string
	Rows    []any
	Timeout time.Duration
}

func runnerOf(sum workbook.RunSummary, err error, calls *[]rrCall) RowRunner {
	return func(_ context.Context, ws, wb string, cols []string, rows []any, to time.Duration) (workbook.RunSummary, error) {
		*calls = append(*calls, rrCall{ws, wb, cols, rows, to})
		return sum, err
	}
}

var abcCols = []Column{colgraph.MakeCol("id", "a", "name", "A"), colgraph.MakeCol("id", "b", "name", "B"), colgraph.MakeCol("id", "c", "name", "C")}

func reAct(ids ...any) map[string]any {
	return map[string]any{"type": "re_enrich", "config": map[string]any{"column_ids": ids}}
}

func TestReEnrichDispatchVectors(t *testing.T) {
	type vec struct {
		Name          string          `json:"name"`
		Action        json.RawMessage `json:"action"`
		RowID         string          `json:"row_id"`
		Columns       json.RawMessage `json:"columns"`
		Scripted      map[string]any  `json:"scripted"`
		ScriptedRaise *string         `json:"scripted_raises"`
		Result        *struct {
			Status     string  `json:"status"`
			Summary    string  `json:"summary"`
			Error      *string `json:"error"`
			SkipReason *string `json:"skip_reason"`
		} `json:"result"`
		EnrichCall *struct {
			WorkbookID      string   `json:"workbook_id"`
			ColumnIDs       []string `json:"column_ids"`
			RowIDs          []any    `json:"row_ids"`
			ProviderTimeout float64  `json:"provider_timeout"`
			WorkspaceID     string   `json:"workspace_id"`
		} `json:"enrich_call"`
	}
	n := 0
	for _, v := range loadVectors[vec](t, "dispatch_vectors.json") {
		if v.Columns == nil || v.Result == nil || !strings.HasPrefix(v.Name, "re_enrich") {
			continue
		}
		n++
		t.Run(v.Name, func(t *testing.T) {
			var sum workbook.RunSummary
			var runErr error
			if v.ScriptedRaise != nil {
				runErr = errors.New(*v.ScriptedRaise)
			} else {
				if f, ok := v.Scripted["completed"].(float64); ok {
					sum.Completed = int(f)
				}
				if f, ok := v.Scripted["errors"].(float64); ok {
					sum.Errors = int(f)
				}
				sum.BudgetExceeded, _ = v.Scripted["budget_exceeded"].(bool)
				sum.Error, _ = v.Scripted["error"].(string)
			}
			var calls []rrCall
			d := Deps{RunRows: runnerOf(sum, runErr, &calls)}
			cols := parseCols(t, v.Columns)
			got := Execute(context.Background(), d, "ws-1", decodeAction(t, v.Action), "wb-1", v.RowID, nil, nil, cols, "i")
			want := Result{Status: v.Result.Status, Summary: v.Result.Summary}
			if v.Result.Error != nil {
				want.Error = *v.Result.Error
			}
			if v.Result.SkipReason != nil {
				want.SkipReason = *v.Result.SkipReason
			}
			if got != want {
				t.Fatalf("got %+v\nwant %+v", got, want)
			}
			if v.EnrichCall == nil {
				if len(calls) != 0 {
					t.Fatalf("run must not start: %+v", calls)
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("calls = %d", len(calls))
			}
			c, e := calls[0], v.EnrichCall
			rows := make([]any, len(e.RowIDs))
			for i, r := range e.RowIDs {
				if f, ok := r.(float64); ok {
					rows[i] = int64(f)
				} else {
					rows[i] = r
				}
			}
			if c.WS != e.WorkspaceID || c.WB != e.WorkbookID || !reflect.DeepEqual(c.Cols, e.ColumnIDs) || !reflect.DeepEqual(c.Rows, rows) ||
				c.Timeout != time.Duration(e.ProviderTimeout*float64(time.Second)) {
				t.Fatalf("run call %+v, want %+v", c, e)
			}
		})
	}
	if n < 19 {
		t.Fatalf("only %d re_enrich vectors ran", n)
	}
}

func TestReEnrichNoColumnsSkipped(t *testing.T) {
	var calls []rrCall
	d := Deps{RunRows: runnerOf(workbook.RunSummary{}, nil, &calls)}
	for _, cfg := range []map[string]any{nil, {}, {"column_ids": []any{}}, {"column_ids": nil}} {
		res := ReEnrich(context.Background(), d, "ws", "wb", "5", cfg, abcCols)
		if res != (Result{Status: "skipped", SkipReason: "no_target", Summary: "no column_ids"}) {
			t.Fatalf("%v: %+v", cfg, res)
		}
	}
	if len(calls) != 0 {
		t.Fatal("run started")
	}
}

func TestReEnrichNoneExistSkipped(t *testing.T) {
	var calls []rrCall
	d := Deps{RunRows: runnerOf(workbook.RunSummary{}, nil, &calls)}
	res := ReEnrich(context.Background(), d, "ws", "wb", "5", map[string]any{"column_ids": []any{"zzz"}}, abcCols)
	if res != (Result{Status: "skipped", SkipReason: "no_target", Summary: "none of the configured columns exist in this workbook"}) || len(calls) != 0 {
		t.Fatalf("%+v %v", res, calls)
	}
}

func TestReEnrichSummaryAndBudgetSuffix(t *testing.T) {
	var calls []rrCall
	d := Deps{RunRows: runnerOf(workbook.RunSummary{Completed: 2, Errors: 0, BudgetExceeded: true}, nil, &calls)}
	res := ReEnrich(context.Background(), d, "ws", "wb", "5", reAct("a", "b")["config"].(map[string]any), abcCols)
	if res != (Result{Status: "success", Summary: "re_enrich: 2 cells completed, 0 errors [workbook budget cap hit]"}) {
		t.Fatalf("%+v", res)
	}
	d = Deps{RunRows: runnerOf(workbook.RunSummary{Completed: 1, Errors: 2}, nil, &calls)}
	res = ReEnrich(context.Background(), d, "ws", "wb", "5", map[string]any{"column_ids": []any{"a"}}, abcCols)
	if res != (Result{Status: "failed", Summary: "re_enrich: 1 cells completed, 2 errors", Error: "2 cell errors"}) {
		t.Fatalf("%+v", res)
	}
}

func TestReEnrichProviderDownIsFailedNotPanic(t *testing.T) {
	d := Deps{RunRows: func(context.Context, string, string, []string, []any, time.Duration) (workbook.RunSummary, error) {
		return workbook.RunSummary{}, errors.New("provider call: dial tcp 127.0.0.1:1: connect: connection refused")
	}}
	res := Execute(context.Background(), d, "ws", reAct("a"), "wb", "5", nil, nil, abcCols, "i")
	if res.Status != "failed" || !strings.Contains(res.Error, "connection refused") || res.SkipReason != "" {
		t.Fatalf("%+v", res)
	}
	// A crash inside the run is a failed result too, never a panic.
	d = Deps{RunRows: func(context.Context, string, string, []string, []any, time.Duration) (workbook.RunSummary, error) {
		panic("kaboom")
	}}
	res = Execute(context.Background(), d, "ws", reAct("a"), "wb", "5", nil, nil, abcCols, "i")
	if res.Status != "failed" || !strings.Contains(res.Error, "kaboom") {
		t.Fatalf("%+v", res)
	}
	// No runner configured: failed, not a nil dereference.
	res = Execute(context.Background(), Deps{}, "ws", reAct("a"), "wb", "5", nil, nil, abcCols, "i")
	if res.Status != "failed" || res.Error == "" {
		t.Fatalf("%+v", res)
	}
}

func TestReEnrichUsesTenOutOfTenTimeout(t *testing.T) {
	var calls []rrCall
	d := Deps{RunRows: runnerOf(workbook.RunSummary{Completed: 1}, nil, &calls)}
	ReEnrich(context.Background(), d, "ws", "wb", "5", map[string]any{"column_ids": []any{"a"}}, abcCols)
	if len(calls) != 1 || calls[0].Timeout != 10*time.Second {
		t.Fatalf("%+v", calls)
	}
}

func TestReEnrichCancelledIsFailedContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := Deps{RunRows: func(context.Context, string, string, []string, []any, time.Duration) (workbook.RunSummary, error) {
		cancel()
		return workbook.RunSummary{Completed: 0, Errors: 1}, ctx.Err()
	}}
	res := ReEnrich(ctx, d, "ws", "wb", "5", map[string]any{"column_ids": []any{"a"}}, abcCols)
	if res.Status != "failed" || res.Error != "context canceled" {
		t.Fatalf("%+v", res)
	}
}

func TestReEnrichErrorTruncatedTo200(t *testing.T) {
	d := Deps{RunRows: func(context.Context, string, string, []string, []any, time.Duration) (workbook.RunSummary, error) {
		return workbook.RunSummary{}, errors.New(strings.Repeat("y", 500))
	}}
	res := ReEnrich(context.Background(), d, "ws", "wb", "5", map[string]any{"column_ids": []any{"a"}}, abcCols)
	if len(res.Error) != 200 {
		t.Fatalf("len = %d", len(res.Error))
	}
}

// ---- dispatcher ------------------------------------------------------------

func TestDispatcherUnsupportedType(t *testing.T) {
	for _, act := range []map[string]any{{"type": "carrier_pigeon"}, {"type": ""}, {"type": nil}, {}} {
		res := Execute(context.Background(), Deps{}, "ws", act, "wb", "1", nil, nil, nil, "i")
		name := "None"
		if s, ok := act["type"].(string); ok {
			name = s
		}
		want := Result{Status: "failed", Error: "action type '" + name + "' not supported", SkipReason: "not_connected"}
		if res != want {
			t.Fatalf("%v: got %+v want %+v", act, res, want)
		}
	}
}

func TestExecuteTenantIsolation(t *testing.T) {
	var calls []rrCall
	d := Deps{RunRows: runnerOf(workbook.RunSummary{Completed: 1}, nil, &calls)}
	for _, ws := range []string{"ws-a", "ws-b"} {
		Execute(context.Background(), d, ws, reAct("a"), "wb-"+ws, "5", nil, nil, abcCols, "i")
	}
	if len(calls) != 2 || calls[0].WS != "ws-a" || calls[0].WB != "wb-ws-a" || calls[1].WS != "ws-b" || calls[1].WB != "wb-ws-b" {
		t.Fatalf("%+v", calls)
	}
}

func TestExecuteRecoversPanic(t *testing.T) {
	d := Deps{Push: func(context.Context, string, map[string]any, map[string]any, string, int64) crmpush.Result {
		panic("crm exploded")
	}}
	res := Execute(context.Background(), d, "ws", map[string]any{"type": "push_crm"}, "", "", nil, nil, nil, "i")
	if res.Status != "failed" || !strings.Contains(res.Error, "crm exploded") {
		t.Fatalf("%+v", res)
	}
}

func TestActionTypeSets(t *testing.T) {
	if !IdempotentTypes["push_crm"] || !IdempotentTypes["re_enrich"] || len(IdempotentTypes) != 2 {
		t.Fatal(IdempotentTypes)
	}
	if !NonIdempotentTypes["webhook"] || !NonIdempotentTypes["send_email"] || len(NonIdempotentTypes) != 2 {
		t.Fatal(NonIdempotentTypes)
	}
}
