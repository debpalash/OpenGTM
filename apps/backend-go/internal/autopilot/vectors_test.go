package autopilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Vectors: testdata/autopilot.json, recorded from legacy Python by internal/copilot/testdata/record_tools.py.

type vectors struct {
	Guard []struct {
		Guard, Text, Sanitized, Guarded, Notice string
	} `json:"guard"`
	Heuristic []struct {
		Goal        string          `json:"goal"`
		TargetCount int64           `json:"target_count"`
		Plan        json.RawMessage `json:"plan"`
	} `json:"heuristic"`
	Validate []struct {
		Caps     string            `json:"caps"`
		Settings map[string]string `json:"settings"`
		Name     string            `json:"name"`
		Input    json.RawMessage   `json:"input"`
		Output   json.RawMessage   `json:"output"`
	} `json:"validate"`
	Describe []struct {
		Plan json.RawMessage `json:"plan"`
		Text string          `json:"text"`
	} `json:"describe"`
	Schema        json.RawMessage `json:"schema"`
	PlannerSystem string          `json:"planner_system"`
	LLM           []struct {
		Name     string            `json:"name"`
		Settings map[string]string `json:"settings"`
		Provider bool              `json:"provider"`
		Memory   []struct {
			WorkspaceID string  `json:"workspace_id"`
			Goal        string  `json:"goal"`
			WorkbookID  *string `json:"workbook_id"`
			Outcome     string  `json:"outcome"`
		} `json:"memory"`
		Goal        string            `json:"goal"`
		TargetCount int64             `json:"target_count"`
		Responses   []json.RawMessage `json:"responses"`
		Requests    []struct {
			Messages     []map[string]string `json:"messages"`
			System       string              `json:"system"`
			OutputFormat json.RawMessage     `json:"output_format"`
			MaxTokens    int                 `json:"max_tokens"`
		} `json:"requests"`
		Plan json.RawMessage `json:"plan"`
	} `json:"llm"`
	Memory []struct {
		Goal       string  `json:"goal"`
		WorkbookID *string `json:"workbook_id"`
		Outcome    string  `json:"outcome"`
	} `json:"memory"`
	Execute []struct {
		Name   string                       `json:"name"`
		Plan   json.RawMessage              `json:"plan"`
		Script map[string][]json.RawMessage `json:"script"`
		Calls  []struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"calls"`
		Result json.RawMessage `json:"result"`
	} `json:"execute"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/autopilot.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// pyDump renders raw recorded JSON as json.dumps would.
func pyDump(t *testing.T, raw []byte) string {
	t.Helper()
	b, err := ssestream.Reformat(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func objDump(t *testing.T, o *wbcrud.Obj) string {
	t.Helper()
	if o == nil {
		return "null"
	}
	b, err := o.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return pyDump(t, b)
}

func parse(t *testing.T, raw []byte) any {
	t.Helper()
	o, err := wbcrud.ParseObjectCanon([]byte(`{"v": ` + string(raw) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := o.Get("v")
	return v
}

type mapSettings map[string]string

func (m mapSettings) Get(_ context.Context, key, def string) (string, error) {
	if v, ok := m[key]; ok {
		return v, nil
	}
	return def, nil
}

func TestGuardMatchesPython(t *testing.T) {
	ctx := context.Background()
	for _, c := range load(t).Guard {
		s := mapSettings{"RESEARCH_PROMPT_GUARD": c.Guard}
		if got := Sanitize(c.Text); got != c.Sanitized {
			t.Errorf("sanitize %q:\n got %q\nwant %q", c.Text, got, c.Sanitized)
		}
		if got := Guard(ctx, s, c.Text, "prior goal"); got != c.Guarded {
			t.Errorf("guard(%s) %q:\n got %q\nwant %q", c.Guard, c.Text, got, c.Guarded)
		}
		if got := SystemNotice(ctx, s); got != c.Notice {
			t.Errorf("notice(%s) = %q", c.Guard, got)
		}
	}
}

func TestHeuristicPlanMatchesPython(t *testing.T) {
	for _, c := range load(t).Heuristic {
		if got, want := objDump(t, HeuristicPlan(c.Goal, c.TargetCount)), pyDump(t, c.Plan); got != want {
			t.Errorf("%q:\n got %s\nwant %s", c.Goal, got, want)
		}
	}
}

func TestValidatePlanMatchesPython(t *testing.T) {
	ctx := context.Background()
	for _, c := range load(t).Validate {
		caps := CapsFrom(ctx, mapSettings(c.Settings))
		got := objDump(t, ValidatePlan(parse(t, c.Input), caps))
		if want := pyDump(t, c.Output); got != want {
			t.Errorf("%s/%s:\n got %s\nwant %s", c.Caps, c.Name, got, want)
		}
	}
}

func TestDescribePlanMatchesPython(t *testing.T) {
	for i, c := range load(t).Describe {
		got, ok := DescribePlan(parse(t, c.Plan).(*wbcrud.Obj))
		if !ok || got != c.Text {
			t.Errorf("%d: ok=%v\n got %q\nwant %q", i, ok, got, c.Text)
		}
	}
}

func TestPlanSchemaAndSystemMatchPython(t *testing.T) {
	v := load(t)
	if got, want := objDump(t, PlanSchema()), pyDump(t, v.Schema); got != want {
		t.Fatalf("schema:\n got %s\nwant %s", got, want)
	}
	if PlannerSystem != v.PlannerSystem {
		t.Fatalf("system prompt differs")
	}
}

type fakeLLM struct {
	provider bool
	script   []json.RawMessage
	requests []llm.AnthropicRequest
}

func (f *fakeLLM) AnthropicProvider(context.Context) (llm.Provider, bool) {
	return llm.Provider{Native: true}, f.provider
}

func (f *fakeLLM) AnthropicTurn(_ context.Context, r llm.AnthropicRequest) (*llm.AnthropicResponse, error) {
	f.requests = append(f.requests, r)
	item := f.script[0]
	f.script = f.script[1:]
	if string(item) == `"raise"` {
		return nil, errors.New("provider down")
	}
	var blocks []map[string]string
	if err := json.Unmarshal(item, &blocks); err != nil {
		return nil, err
	}
	resp := &llm.AnthropicResponse{}
	for _, b := range blocks {
		raw, _ := json.Marshal(b)
		resp.Content = append(resp.Content, raw)
	}
	return resp, nil
}

func TestPlannerMatchesPython(t *testing.T) {
	ctx := context.Background()
	for _, c := range load(t).LLM {
		t.Run(c.Name, func(t *testing.T) {
			f := &fakeLLM{provider: c.Provider, script: c.Responses}
			clock := time.Unix(1000, 0)
			mem := &InProcessMemory{Now: func() time.Time { clock = clock.Add(time.Second); return clock }}
			for _, m := range c.Memory {
				if err := mem.Record(ctx, m.WorkspaceID, m.Goal, m.WorkbookID, m.Outcome); err != nil {
					t.Fatal(err)
				}
			}
			p := &Planner{LLM: f, Settings: mapSettings(c.Settings), Memory: mem}
			plan := p.Draft(ctx, c.Goal, c.TargetCount, "ws-1")
			if got, want := objDump(t, plan), pyDump(t, c.Plan); got != want {
				t.Fatalf("plan:\n got %s\nwant %s", got, want)
			}
			if len(f.requests) != len(c.Requests) {
				t.Fatalf("requests %d, want %d", len(f.requests), len(c.Requests))
			}
			for i, r := range f.requests {
				want := c.Requests[i]
				msg := r.Messages[0].(map[string]any)
				if msg["role"] != "user" || msg["content"] != want.Messages[0]["content"] {
					t.Errorf("user prompt:\n got %q\nwant %q", msg["content"], want.Messages[0]["content"])
				}
				if r.System != want.System {
					t.Errorf("system:\n got %q\nwant %q", r.System, want.System)
				}
				of, _ := json.Marshal(r.OutputFormat)
				var a, b any
				_ = json.Unmarshal(of, &a)
				_ = json.Unmarshal(want.OutputFormat, &b)
				ab, _ := json.Marshal(a)
				bb, _ := json.Marshal(b)
				if !bytes.Equal(ab, bb) || r.MaxTokens != want.MaxTokens {
					t.Errorf("output format / max tokens: %s %d", of, r.MaxTokens)
				}
			}
		})
	}
}

func TestMemoryMatchesPython(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(0, 0)
	m := &InProcessMemory{Now: func() time.Time { clock = clock.Add(time.Millisecond); return clock }}
	for i := 0; i < 55; i++ {
		wb := "wb-" + itoa(i%52)
		outcome := "failed"
		if i%2 == 1 {
			outcome = "ok"
		}
		_ = m.Record(ctx, "ws-m", "goal "+itoa(i), &wb, outcome)
	}
	_ = m.Record(ctx, "ws-m", "no wb", nil, "ok")
	_ = m.Record(ctx, "", "ignored", nil, "ok")
	got, _ := m.Recent(ctx, "ws-m", 60)
	want := load(t).Memory
	if len(got) != len(want) {
		t.Fatalf("len %d, want %d", len(got), len(want))
	}
	for i := range want {
		g := got[i]
		if g.Goal != want[i].Goal || g.Outcome != want[i].Outcome || (g.WorkbookID == nil) != (want[i].WorkbookID == nil) ||
			(g.WorkbookID != nil && *g.WorkbookID != *want[i].WorkbookID) {
			t.Fatalf("row %d: %+v, want %+v", i, g, want[i])
		}
	}
}

func itoa(i int) string { return str(int64(i)) }

func TestExecuteMatchesPython(t *testing.T) {
	ctx := context.Background()
	for _, c := range load(t).Execute {
		t.Run(c.Name, func(t *testing.T) {
			script := map[string][]json.RawMessage{}
			for k, v := range c.Script {
				script[k] = append([]json.RawMessage(nil), v...)
			}
			var calls []string
			tool := func(_ context.Context, name string, args map[string]any) (string, error) {
				b, _ := json.Marshal(args)
				calls = append(calls, name+" "+string(b))
				if l := script[name]; len(l) > 0 {
					script[name] = l[1:]
					return string(l[0]), nil
				}
				return `{"ok": true}`, nil
			}
			res, err := Execute(ctx, parse(t, c.Plan).(*wbcrud.Obj), tool)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := objDump(t, res), pyDump(t, c.Result); got != want {
				t.Fatalf("result:\n got %s\nwant %s", got, want)
			}
			if len(calls) != len(c.Calls) {
				t.Fatalf("calls %v, want %d", calls, len(c.Calls))
			}
			for i, w := range c.Calls {
				var a any
				_ = json.Unmarshal(w.Args, &a)
				b, _ := json.Marshal(a)
				if want := w.Name + " " + string(b); calls[i] != want {
					t.Errorf("call %d: %s, want %s", i, calls[i], want)
				}
			}
		})
	}
}

func TestMemoryPlanStoreSingleUse(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(0, 0)
	s := &MemoryPlanStore{Now: func() time.Time { return now }}
	testPlanStore(t, ctx, s, func() {}, func() { now = now.Add(DefaultPlanTTL + 2*time.Second) })
}

// testPlanStore checks autopilot_plan_store semantics on any PlanStore; shortTTL shortens the TTL of later
// puts and advance moves past it.
func testPlanStore(t *testing.T, ctx context.Context, s PlanStore, shortTTL, advance func()) {
	t.Helper()
	id, nonce, err := s.Put(ctx, "ws-1", "7", []byte(`{"goal": "g"}`))
	if err != nil || id == "" || nonce == "" || id == nonce {
		t.Fatalf("put: %q %q %v", id, nonce, err)
	}
	if p, _ := s.Peek(ctx, "ws-2", id); p != nil {
		t.Fatal("peek from another workspace")
	}
	if p, _ := s.Peek(ctx, "ws-1", id); string(p) != `{"goal": "g"}` {
		t.Fatalf("peek %q", p)
	}
	for _, bad := range [][4]string{{"ws-1", "7", id, "wrong"}, {"ws-2", "7", id, nonce}, {"ws-1", "8", id, nonce},
		{"ws-1", "", id, nonce}, {"ws-1", "7", "", nonce}, {"ws-1", "7", id, ""}, {"ws-1", "7", "missing", nonce}} {
		if p, err := s.Consume(ctx, bad[0], bad[1], bad[2], bad[3]); p != nil || err != nil {
			t.Fatalf("consume %v: %q %v", bad, p, err)
		}
	}
	if p, err := s.Consume(ctx, "ws-1", "7", id, nonce); string(p) != `{"goal": "g"}` || err != nil {
		t.Fatalf("consume: %q %v", p, err)
	}
	if p, _ := s.Consume(ctx, "ws-1", "7", id, nonce); p != nil {
		t.Fatal("consumed twice")
	}
	if p, _ := s.Peek(ctx, "ws-1", id); p == nil {
		t.Fatal("a consumed plan still renders until it expires")
	}
	shortTTL()
	id2, nonce2, _ := s.Put(ctx, "ws-1", "", []byte(`{}`))
	advance()
	if p, _ := s.Consume(ctx, "ws-1", "", id2, nonce2); p != nil {
		t.Fatal("expired plan consumed")
	}
}
