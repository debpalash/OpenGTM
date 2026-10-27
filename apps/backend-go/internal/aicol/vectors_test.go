package aicol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
)

type vectors struct {
	Guard struct {
		Cases []struct {
			Input, Guard, Sanitized, Wrapped, Guarded, Notice string
			Enabled                                           bool
		}
		Flags  map[string]bool
		System string
	}
	Prompts struct {
		Cols  map[string]json.RawMessage
		Rows  []json.RawMessage
		Cases []struct {
			Cols, Template, Guard string
			Row                   int
			Prompt                map[string]any
			LeadValues            map[string]any `json:"lead_values"`
		}
	}
	AI       []scenario
	Research []scenario
	RPC      []scenario `json:"rpc"`
	Batch    []struct {
		Anthropic bool
		Columns   json.RawMessage
		Requests  []struct {
			CustomID string          `json:"custom_id"`
			Row      json.RawMessage `json:"row"`
			ColumnID string          `json:"column_id"`
		}
		Calls []map[string]any
	}
	Constants struct {
		NativeSystem string         `json:"native_system"`
		ReactSystem  string         `json:"react_system"`
		Notice       string         `json:"notice"`
		SearchTool   map[string]any `json:"search_tool"`
		FetchTool    map[string]any `json:"fetch_tool"`
		Schema       map[string]any `json:"schema"`
	}
}

type scenario struct {
	Name      string
	Prompt    string
	Row       json.RawMessage
	Cols      string
	Settings  map[string]string
	Anthropic bool
	LLM       []map[string]json.RawMessage `json:"llm"`
	Search    map[string]json.RawMessage
	Pages     map[string]map[string]string
	Blocked   map[string]string
	MaxSteps  *int     `json:"max_steps"`
	Budget    *float64 `json:"budget"`
	Data      map[string]json.RawMessage
	Calls     []any
	Fields    map[string]any
	Result    map[string]any
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func decodeRow(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func decodeCols(t *testing.T, raw []byte) []colgraph.Col {
	t.Helper()
	v, err := rowtmpl.DecodeOrdered(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out []colgraph.Col
	for _, e := range v.([]any) {
		out = append(out, e.(colgraph.Col))
	}
	return out
}

// generic re-decodes a value through JSON so Go and Python shapes compare structurally.
func generic(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type mapSettings map[string]string

func (m mapSettings) Get(_ context.Context, key, def string) (string, error) {
	if v, ok := m[key]; ok {
		return v, nil
	}
	return def, nil
}

// fakeWorld replays one recorded scenario and records the calls in the recorder's shape.
type fakeWorld struct {
	t     *testing.T
	sc    scenario
	calls []any
	i     int
}

var testProv = llm.Provider{ID: "anthropic", APIKey: "sk-test", Model: "claude-sonnet-4-6", Native: true}

func (f *fakeWorld) next(kind string) map[string]json.RawMessage {
	if f.i >= len(f.sc.LLM) {
		f.t.Fatalf("%s: llm script exhausted at call %d (%s)", f.sc.Name, f.i, kind)
	}
	r := f.sc.LLM[f.i]
	f.i++
	var k string
	_ = json.Unmarshal(r["kind"], &k)
	if k != "" && k != kind {
		f.t.Fatalf("%s: llm call %d: expected %s got %s", f.sc.Name, f.i-1, k, kind)
	}
	return r
}

func (f *fakeWorld) Complete(_ context.Context, req llm.CompleteRequest) (string, error) {
	f.calls = append(f.calls, map[string]any{"fn": "complete", "prompt": req.Prompt, "system": req.System, "max_tokens": req.MaxTokens})
	var s string
	_ = json.Unmarshal(f.next("complete")["text"], &s)
	if s == "" {
		return "", llm.ErrEmptyResponse
	}
	return s, nil
}

func (f *fakeWorld) ExtractJSON(_ context.Context, prompt, system string, maxTokens int) (map[string]any, error) {
	f.calls = append(f.calls, map[string]any{"fn": "extract_json", "prompt": prompt, "system": system, "max_tokens": maxTokens})
	r := f.next("extract_json")
	m := decodeRow(f.t, r["json"])
	if len(m) == 0 {
		return nil, llm.ErrInvalidJSON
	}
	return m, nil
}

func (f *fakeWorld) AnthropicProvider(context.Context) (llm.Provider, bool) {
	return testProv, f.sc.Anthropic
}

func (f *fakeWorld) AnthropicTurn(_ context.Context, r llm.AnthropicRequest) (*llm.AnthropicResponse, error) {
	var tools any
	if len(r.Tools) > 0 {
		tools = r.Tools
	}
	f.calls = append(f.calls, map[string]any{"fn": "anthropic_tool_call", "messages": r.Messages, "system": r.System,
		"tools": tools, "tool_choice": r.ToolChoice, "output_format": r.OutputFormat, "max_tokens": r.MaxTokens,
		"model": r.Provider.Model})
	s := f.next("turn")
	if e, ok := s["raise"]; ok {
		var msg string
		_ = json.Unmarshal(e, &msg)
		return nil, errors.New(msg)
	}
	resp := &llm.AnthropicResponse{StopReason: "end_turn"}
	if sr, ok := s["stop_reason"]; ok {
		_ = json.Unmarshal(sr, &resp.StopReason)
	}
	_ = json.Unmarshal(s["content"], &resp.Content)
	if u, ok := s["usage"]; ok {
		_ = json.Unmarshal(u, &resp.Usage)
	}
	return resp, nil
}

func (f *fakeWorld) BatchComplete(context.Context, []llm.BatchRequest, llm.BatchOptions) (map[string]string, error) {
	return nil, errors.New("unused")
}

func (f *fakeWorld) Search(_ context.Context, q string, n int) []Hit {
	f.calls = append(f.calls, map[string]any{"fn": "search", "query": q, "max_results": n})
	raw, ok := f.sc.Search[q]
	if !ok || string(raw) == `"raise"` {
		return nil
	}
	var hs []struct{ Title, Href, Body string }
	_ = json.Unmarshal(raw, &hs)
	out := make([]Hit, len(hs))
	for i, h := range hs {
		out[i] = Hit(h)
	}
	return out
}

func (f *fakeWorld) Fetch(_ context.Context, u string) (string, error) {
	f.calls = append(f.calls, map[string]any{"fn": "fetch", "url": u})
	p, ok := f.sc.Pages[u]
	if !ok {
		return "", errors.New("connection refused")
	}
	if e, ok := p["raise"]; ok {
		return "", errors.New(e)
	}
	return truncRunes(p["text"], 2000), nil
}

func (f *fakeWorld) deps() Deps {
	return Deps{LLM: f, Settings: mapSettings(f.sc.Settings), Search: f, Pages: f,
		URLCheck: func(u string) error {
			if r := f.sc.Blocked[u]; r != "" {
				return errors.New(r)
			}
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }}
}

func checkCalls(t *testing.T, name string, want []any, got []any) {
	t.Helper()
	if want == nil {
		want = []any{}
	}
	g := generic(t, got)
	if g == nil {
		g = []any{}
	}
	if !reflect.DeepEqual(generic(t, want), g) {
		wb, _ := json.MarshalIndent(want, "", " ")
		gb, _ := json.MarshalIndent(g, "", " ")
		t.Errorf("%s: calls differ\nwant %s\ngot  %s", name, wb, gb)
	}
}

func resultFields(t *testing.T, r Result, withMeta bool) any {
	f := map[string]any{"value": r.Value, "error": nil}
	if r.Error != "" {
		f["error"] = r.Error
	}
	if withMeta {
		if r.Metadata != nil {
			f["metadata"] = r.Metadata
		} else {
			f["metadata"] = nil
		}
	}
	return generic(t, f)
}

func TestGuardVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Guard.Cases {
		g := Guard{On: GuardEnabled(c.Guard)}
		if g.On != c.Enabled {
			t.Fatalf("enabled %q", c.Guard)
		}
		if got := Sanitize(c.Input); got != c.Sanitized {
			t.Errorf("sanitize(%q)\nwant %q\ngot  %q", c.Input, c.Sanitized, got)
		}
		if got := Wrap(c.Input, "x result"); got != c.Wrapped {
			t.Errorf("wrap(%q)", c.Input)
		}
		if got := g.Untrusted(c.Input, "row data"); got != c.Guarded {
			t.Errorf("guard(%q, %s)\nwant %q\ngot  %q", c.Input, c.Guard, c.Guarded, got)
		}
		if g.Notice() != c.Notice {
			t.Errorf("notice %s", c.Guard)
		}
	}
	for in, want := range v.Guard.Flags {
		if GuardEnabled(in) != want {
			t.Errorf("flag %q", in)
		}
	}
	if v.Guard.System != AIColumnSystem || v.Constants.Notice != UntrustedNotice ||
		v.Constants.NativeSystem != nativeSystem || v.Constants.ReactSystem != reactSystem {
		t.Fatal("system prompt constants differ from Python")
	}
	for name, pair := range map[string][2]any{"search": {v.Constants.SearchTool, searchToolDef()},
		"fetch": {v.Constants.FetchTool, fetchToolDef()}, "schema": {v.Constants.Schema, researchSchema()}} {
		if !reflect.DeepEqual(generic(t, pair[0]), generic(t, pair[1])) {
			t.Errorf("%s definition differs", name)
		}
	}
}

func TestPromptVectors(t *testing.T) {
	v := loadVectors(t)
	cols := map[string][]colgraph.Col{}
	for k, raw := range v.Prompts.Cols {
		cols[k] = decodeCols(t, raw)
	}
	for _, c := range v.Prompts.Cases {
		row := decodeRow(t, v.Prompts.Rows[c.Row])
		got, err := BuildAIPrompt(c.Template, row, cols[c.Cols], Guard{On: GuardEnabled(c.Guard)})
		var gv map[string]any
		if err != nil {
			gv = map[string]any{"error": "ValueError: " + err.Error()}
		} else {
			gv = map[string]any{"ok": got}
		}
		if !reflect.DeepEqual(gv, c.Prompt) {
			t.Errorf("%s/%d %q guard=%s\nwant %v\ngot  %v", c.Cols, c.Row, c.Template, c.Guard, c.Prompt, gv)
		}
		lv := rowtmpl.LeadValues(row, cols[c.Cols])
		if !reflect.DeepEqual(generic(t, map[string]any{"ok": lv}), generic(t, c.LeadValues)) {
			t.Errorf("lead values %s/%d", c.Cols, c.Row)
		}
	}
}

func TestAIFormulaVectors(t *testing.T) {
	v := loadVectors(t)
	for _, sc := range v.AI {
		colsRaw := v.Prompts.Cols["cols"]
		if sc.Cols == "dup" {
			colsRaw = v.Prompts.Cols["dup"]
		}
		f := &fakeWorld{t: t, sc: sc}
		res, err := AIFormula(context.Background(), f.deps(), sc.Prompt, decodeRow(t, sc.Row), decodeCols(t, colsRaw))
		if err != nil {
			t.Fatal(err)
		}
		checkCalls(t, sc.Name, sc.Calls, f.calls)
		if got := resultFields(t, res, false); !reflect.DeepEqual(got, generic(t, sc.Fields)) {
			t.Errorf("%s: fields want %v got %v", sc.Name, sc.Fields, got)
		}
	}
}

func TestResearchVectors(t *testing.T) {
	v := loadVectors(t)
	cols := decodeCols(t, v.Prompts.Cols["cols"])
	for _, sc := range v.Research {
		f := &fakeWorld{t: t, sc: sc}
		in := ResearchInput{Template: sc.Prompt, Row: decodeRow(t, sc.Row), Cols: cols, MaxSteps: 4, Budget: sc.Budget}
		if sc.MaxSteps != nil {
			in.MaxSteps = *sc.MaxSteps
		}
		res, err := Research(context.Background(), f.deps(), in)
		if err != nil {
			t.Fatal(err)
		}
		checkCalls(t, sc.Name, sc.Calls, f.calls)
		if got := resultFields(t, res, true); !reflect.DeepEqual(got, generic(t, sc.Fields)) {
			t.Errorf("%s: fields\nwant %v\ngot  %v", sc.Name, sc.Fields, got)
		}
		if f.i != len(sc.LLM) {
			t.Errorf("%s: %d of %d scripted llm answers used", sc.Name, f.i, len(sc.LLM))
		}
	}
}

func TestPlaybookResearchVectors(t *testing.T) {
	v := loadVectors(t)
	typed := map[string]bool{"bad_context": true, "bad_steps": true, "bad_budget": true} // impossible with a typed request
	for _, sc := range v.RPC {
		if typed[sc.Name] {
			continue
		}
		f := &fakeWorld{t: t, sc: sc}
		req := PlaybookRequest{MaxSteps: 4, OutputFormat: "text"}
		_ = json.Unmarshal(sc.Data["prompt_template"], &req.PromptTemplate)
		if raw, ok := sc.Data["context"]; ok && string(raw) != "null" {
			req.Context = decodeRow(t, raw)
		}
		if raw, ok := sc.Data["max_steps"]; ok {
			_ = json.Unmarshal(raw, &req.MaxSteps)
		}
		if raw, ok := sc.Data["output_format"]; ok {
			_ = json.Unmarshal(raw, &req.OutputFormat)
		}
		if raw, ok := sc.Data["cell_budget_usd"]; ok {
			_ = json.Unmarshal(raw, &req.CellBudgetUSD)
		}
		res, err := PlaybookResearch(context.Background(), f.deps(), req)
		if err != nil {
			t.Fatal(err)
		}
		checkCalls(t, sc.Name, sc.Calls, f.calls)
		got := map[string]any{"success": res.Success, "value": res.Value, "metadata": res.Metadata, "error": nil}
		if res.Error != "" {
			got["error"] = res.Error
		}
		if !reflect.DeepEqual(generic(t, got), generic(t, sc.Result)) {
			t.Errorf("%s: want %v got %v", sc.Name, sc.Result, generic(t, got))
		}
	}
}

func TestBatchRequestVectors(t *testing.T) {
	v := loadVectors(t)
	b := v.Batch[0]
	var cells []BatchCell
	for _, r := range b.Requests {
		cells = append(cells, BatchCell{CustomID: r.CustomID, Row: decodeRow(t, r.Row), ColumnID: r.ColumnID})
	}
	got, err := BatchRequests(context.Background(), Deps{}, cells, decodeCols(t, b.Columns))
	if err != nil {
		t.Fatal(err)
	}
	var reqs []any
	for _, r := range got {
		reqs = append(reqs, map[string]any{"custom_id": r.CustomID, "prompt": r.Prompt, "system": r.System, "max_tokens": r.MaxTokens})
	}
	if !reflect.DeepEqual(generic(t, reqs), generic(t, b.Calls[0]["requests"])) {
		t.Errorf("batch requests\nwant %v\ngot  %v", b.Calls[0]["requests"], generic(t, reqs))
	}
}
