package nlcolumn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colcompute"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type vectors struct {
	Funcs      []string `json:"funcs"`
	StrMethods []string `json:"str_methods"`
	DSL        string   `json:"dsl_reference"`
	System     string   `json:"system"`
	Cases      []struct {
		Name        string            `json:"name"`
		Instruction string            `json:"instruction"`
		Columns     json.RawMessage   `json:"columns"`
		LLM         []json.RawMessage `json:"llm"`
		Calls       []struct {
			Prompt    string `json:"prompt"`
			System    string `json:"system"`
			MaxTokens int    `json:"max_tokens"`
		} `json:"calls"`
		Result struct {
			Column      json.RawMessage `json:"column"`
			Kind        string          `json:"kind"`
			Explanation string          `json:"explanation"`
		} `json:"result"`
		Error     string `json:"error"`
		Exception string `json:"exception"`
	} `json:"cases"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/nl_column.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

type call struct {
	prompt, system string
	max            int
}

type fakeLLM struct {
	script []json.RawMessage
	calls  []call
}

func (f *fakeLLM) ExtractJSON(_ context.Context, prompt, system string, max int) (*wbcrud.Obj, error) {
	f.calls = append(f.calls, call{prompt, system, max})
	if len(f.script) == 0 {
		return wbcrud.NewObj(), nil
	}
	r := f.script[0]
	f.script = f.script[1:]
	o, err := wbcrud.ParseObjectCanon(r)
	if err != nil {
		return nil, errors.New("not an object") // the Go client never returns a non-object
	}
	return o, nil
}

func TestDSLReference(t *testing.T) {
	v := load(t)
	if DSLReference() != v.DSL {
		t.Errorf("DSL reference drifted from Python:\n%s", DSLReference())
	}
	if System != v.System {
		t.Error("system prompt drifted from Python")
	}
	if !reflect.DeepEqual(formulaFuncs, v.Funcs) || !reflect.DeepEqual(formulaStrMethods, v.StrMethods) {
		t.Errorf("whitelists drifted: %v %v", formulaFuncs, formulaStrMethods)
	}
}

// TestWhitelistsAccepted checks the evaluator still accepts every listed function and method.
func TestWhitelistsAccepted(t *testing.T) {
	for _, f := range formulaFuncs {
		arg := `"a"`
		if f == "default" || f == "concat" {
			arg = `"a", "b"`
		}
		if f == "replace" {
			arg = `"a", "a", "b"`
		}
		if _, err := colcompute.EvalFormula(f+"("+arg+")", nil); err != nil && strings.Contains(err.Error(), "not allowed") {
			t.Errorf("function %s rejected: %v", f, err)
		}
	}
	for _, m := range formulaStrMethods {
		arg := ""
		switch m {
		case "split", "startswith", "endswith", "find", "join", "lstrip", "rstrip":
			arg = `"a"`
		case "replace":
			arg = `"a", "b"`
		case "zfill":
			arg = "3"
		}
		if _, err := colcompute.EvalFormula(`"a".`+m+"("+arg+")", nil); err != nil && strings.Contains(err.Error(), "not allowed") {
			t.Errorf("method %s rejected: %v", m, err)
		}
	}
}

func TestVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Exception != "" {
				t.Skip("Python let the fake LLM's exception propagate (500); the Go client reports errors as empty answers")
			}
			cols, err := wbcrud.ParseColumns(orNull(c.Columns))
			if err != nil {
				t.Fatal(err)
			}
			f := &fakeLLM{script: c.LLM}
			res, err := Generate(context.Background(), f, c.Instruction, cols)
			if c.Error != "" {
				var e *Error
				if !errors.As(err, &e) || e.Msg != c.Error {
					t.Fatalf("error = %v, want %q", err, c.Error)
				}
			} else if err != nil {
				t.Fatal(err)
			} else {
				got, _ := json.Marshal(res.Column)
				if !jsonEqual(t, got, c.Result.Column, c.Name) {
					t.Errorf("column\n got %s\nwant %s", got, c.Result.Column)
				}
				if res.Kind != c.Result.Kind || res.Explanation != c.Result.Explanation {
					t.Errorf("kind/explanation = %q/%q, want %q/%q", res.Kind, res.Explanation, c.Result.Kind, c.Result.Explanation)
				}
			}
			if len(f.calls) != len(c.Calls) {
				t.Fatalf("%d LLM calls, want %d", len(f.calls), len(c.Calls))
			}
			for i, w := range c.Calls {
				g := f.calls[i]
				if g.prompt != w.Prompt || g.system != w.System || g.max != w.MaxTokens {
					t.Errorf("call %d differs:\n got %q\nwant %q", i, g.prompt, w.Prompt)
				}
			}
		})
	}
}

func orNull(b json.RawMessage) []byte {
	if string(b) == "null" || len(b) == 0 {
		return []byte("[]")
	}
	return b
}

// jsonEqual compares two JSON documents textually after compaction, so every nested key order counts.
func jsonEqual(t *testing.T, a, b []byte, _ string) bool {
	t.Helper()
	var x, y bytes.Buffer
	if json.Compact(&x, a) != nil || json.Compact(&y, b) != nil {
		return false
	}
	return x.String() == y.String()
}

func TestPyTitleAndSlug(t *testing.T) {
	for in, want := range map[string]string{"don't stop": "Don'T Stop", "a1b c": "A1B C", "ǆx": "ǅx", "": ""} {
		if got := pyTitle(in); got != want {
			t.Errorf("title(%q) = %q, want %q", in, got, want)
		}
	}
	if got := makeID("İx", nil); got != "i_x" {
		t.Errorf("makeID(İx) = %q", got)
	}
}

func TestContextCancelledIsNotAnEmptyAnswer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Generate(ctx, cancelLLM{}, "do something", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

type cancelLLM struct{}

func (cancelLLM) ExtractJSON(ctx context.Context, _, _ string, _ int) (*wbcrud.Obj, error) {
	return nil, ctx.Err()
}

func TestHTTPKeyOrderPreserved(t *testing.T) {
	f := &fakeLLM{script: []json.RawMessage{json.RawMessage(`{"kind":"http","name":"T","http_url":"https://x.test","http_method":"post","http_headers":{"Zed":"1","Alpha":2},"http_body":{"z":1,"a":{"y":1,"b":2}}}`)}}
	res, err := Generate(context.Background(), f, "call it", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(res.Column)
	if !strings.Contains(string(got), `"http_headers":{"Zed":"1","Alpha":"2"},"http_body":{"z":1,"a":{"y":1,"b":2}}`) {
		t.Errorf("key order lost: %s", got)
	}
}
