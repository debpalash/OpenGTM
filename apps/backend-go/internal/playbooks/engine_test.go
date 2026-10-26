package playbooks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

func TestPyResearcherCallShape(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.UseNumber()
		_ = d.Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","output":{"success":true,"value":{"b":1,"a":[1.5,"x"]},"metadata":{"k":"v"},"error":null},"error":null,"cost_usd":0}`))
	}))
	defer srv.Close()
	c := pyworker.New(srv.URL, "tok-secret", nil)
	r := NewPyResearcher(c, 90*time.Second)
	res, err := r.Research(context.Background(), "ws-real", ResearchRequest{
		CallID: "run1:42:s1:3", PromptTemplate: "find <b>{{name}}</b>", Context: map[string]any{"name": "Ada", "workspace_id": "ws-evil"},
		MaxSteps: 5, OutputFormat: "json", CellBudgetUSD: 0.25,
	})
	if err != nil || !res.Success || res.Error != "" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if auth != "Bearer tok-secret" {
		t.Fatalf("auth header %q", auth)
	}
	if got["provider"] != "research_column" || got["call_id"] != "run1:42:s1:3" || got["workspace_id"] != "ws-real" {
		t.Fatalf("envelope %v", got)
	}
	if got["timeout_s"].(json.Number).String() != "90" {
		t.Fatalf("timeout %v", got["timeout_s"])
	}
	in := got["input"].(map[string]any)
	if _, has := in["workspace_id"]; has {
		t.Fatalf("workspace_id leaked into input: %v", in)
	}
	if in["prompt_template"] != "find <b>{{name}}</b>" || in["output_format"] != "json" ||
		in["max_steps"].(json.Number).String() != "5" || in["cell_budget_usd"].(json.Number).String() != "0.25" {
		t.Fatalf("input %v", in)
	}
	ctxIn := in["context"].(map[string]any)
	if ctxIn["name"] != "Ada" {
		t.Fatalf("context %v", ctxIn)
	}
	// value keeps Python key order and number formatting.
	if v := string(raw(res.Value)); v != `{"b":1,"a":[1.5,"x"]}` {
		t.Fatalf("value %s", v)
	}
	if res.Metadata["k"] != "v" {
		t.Fatalf("metadata %v", res.Metadata)
	}
}

func TestPyResearcherFailureMapping(t *testing.T) {
	status := "error"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch status {
		case "bad":
			_, _ = w.Write([]byte(`{"status":"ok","output":[1,2],"error":null,"cost_usd":0}`))
		default:
			_, _ = w.Write([]byte(`{"status":"` + status + `","output":null,"error":"boom","cost_usd":0}`))
		}
	}))
	defer srv.Close()
	r := NewPyResearcher(pyworker.New(srv.URL, "t", nil), time.Second)
	for _, st := range []string{"error", "timeout", "bad"} {
		status = st
		res, err := r.Research(context.Background(), "ws", ResearchRequest{PromptTemplate: "p", OutputFormat: "text", MaxSteps: 1})
		if err != nil || res.Success || res.Error == "" {
			t.Fatalf("%s: %+v %v", st, res, err)
		}
	}
}

func TestPyResearcherUnavailable(t *testing.T) {
	r := NewPyResearcher(pyworker.New("", "", nil), time.Second)
	_, err := r.Research(context.Background(), "ws", ResearchRequest{PromptTemplate: "p"})
	if err == nil || err.Error() == "" || !isUnavailable(err) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestPyStrMatchesPython(t *testing.T) {
	o := newObj()
	o.set("b", json.Number("1"))
	o.set("a", []any{true, nil, "it's", json.Number("1.5")})
	for _, c := range []struct {
		in   any
		want string
	}{
		{nil, ""}, {"", ""}, {"text", "text"}, {false, ""}, {true, "True"}, {json.Number("0"), ""},
		{json.Number("0.0"), ""}, {json.Number("7"), "7"}, {[]any{}, ""}, {newObj(), ""},
		{o, `{'b': 1, 'a': [True, None, "it's", 1.5]}`},
		{map[string]any{"x": "y"}, `{'x': 'y'}`},
	} {
		if got := valueText(c.in); got != c.want {
			t.Errorf("valueText(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCapUTF8(t *testing.T) {
	s := strings.Repeat("é", 40000) // 80000 bytes
	got := capBytes(s, 64*1024)
	if len(got) > 64*1024 || len(got) < 64*1024-1 || !strings.HasSuffix(got, "é") {
		t.Fatalf("len %d", len(got))
	}
	if capBytes("abc", 64*1024) != "abc" {
		t.Fatal("short string changed")
	}
}

// TestRunAgainstPythonResearchColumn needs a Python worker (TEST_PY_WORKER_URL,
// TEST_PY_WORKER_TOKEN); validation failures answer without calling an LLM.
func TestRunAgainstPythonResearchColumn(t *testing.T) {
	url := os.Getenv("TEST_PY_WORKER_URL")
	if url == "" {
		t.Skip("TEST_PY_WORKER_URL not set")
	}
	r := NewPyResearcher(pyworker.New(url, os.Getenv("TEST_PY_WORKER_TOKEN"), nil), 30*time.Second)
	res, err := r.Research(context.Background(), "ws-py-contract", ResearchRequest{CallID: "t11:1:s:1", PromptTemplate: "  ", OutputFormat: "text", MaxSteps: 1})
	if err != nil || res.Success || res.Error != "prompt_template required" {
		t.Fatalf("empty template: %+v %v", res, err)
	}
	res, err = r.Research(context.Background(), "ws-py-contract", ResearchRequest{CallID: "t11:1:s:2", PromptTemplate: "x", OutputFormat: "xml", MaxSteps: 1})
	if err != nil || res.Success || res.Error != "output_format must be text or json" {
		t.Fatalf("bad format: %+v %v", res, err)
	}
}
