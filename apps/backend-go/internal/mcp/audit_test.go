package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type redactVector struct {
	Input  map[string]any `json:"input"`
	Output map[string]any `json:"output"`
}

func scalarOnly(m map[string]any) bool {
	for _, v := range m {
		switch v.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

func TestRedactVectorsMatchPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/redact_arguments.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []redactVector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	n := 0
	for i, v := range vs {
		if !scalarOnly(v.Input) {
			continue // nested shapes deviate on purpose (D6): TestRedactNestedSecretsAndLists
		}
		n++
		got := RedactArguments(Args(v.Input))
		gj, _ := json.Marshal(got)
		var g any
		_ = json.Unmarshal(gj, &g)
		if !reflect.DeepEqual(g, any(v.Output)) {
			t.Fatalf("vector %d: got %s", i, gj)
		}
	}
	if n < 10 {
		t.Fatalf("only %d top-level vectors exercised", n)
	}
}

func TestRedactNestedSecretsAndLists(t *testing.T) {
	in := Args{
		"actions": []any{map[string]any{"type": "webhook", "config": map[string]any{
			"url": "https://h.example.com", "api_key": "FAKE_NESTED", "ApiKey": "FAKE2", "Set-Cookie": "c",
			"client_credentials": "x", "Authorization": "Bearer FAKE", "email": "keep@example.com"}}},
		"nested":  map[string]any{"password": "p", "deep": map[string]any{"token": "t", "ok": 1.0}},
		"x_token": "top-level-substring",
		"email":   "a@b.example.com",
		"emails":  []any{"keep"},
	}
	out := RedactArguments(in)
	b, _ := json.Marshal(out)
	s := string(b)
	for _, leak := range []string{"FAKE_NESTED", "FAKE2", "Bearer FAKE", `"p"`, `"t"`, "top-level-substring", "a@b.example.com", `"c"`} {
		if strings.Contains(s, leak) {
			t.Fatalf("leak %q in %s", leak, s)
		}
	}
	for _, keep := range []string{"https://h.example.com", "keep@example.com", `"ok":1`, `"emails":["keep"]`} {
		if !strings.Contains(s, keep) {
			t.Fatalf("lost %q in %s", keep, s)
		}
	}
	if in["x_token"] != "top-level-substring" {
		t.Fatal("input mutated")
	}
	// Depth bound: a secret below depth 4 must never be emitted verbatim.
	deep := map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": map[string]any{"e": map[string]any{"token": "DEEPSECRET", "v": "DEEPPLAIN"}}}}}}
	db, _ := json.Marshal(RedactArguments(Args(deep)))
	if strings.Contains(string(db), "DEEPSECRET") {
		t.Fatalf("deep secret leaked: %s", db)
	}
	if RedactArguments(nil) == nil || len(RedactArguments(nil)) != 0 {
		t.Fatal("nil args must give an empty map")
	}
}

func TestRedactTruncatesLongStrings(t *testing.T) {
	long := strings.Repeat("é", 501)
	out := RedactArguments(Args{"notes": long, "list": []any{long}, "m": map[string]any{"k": long}, "short": strings.Repeat("x", 500)})
	want := strings.Repeat("é", 500) + "…"
	if out["notes"] != want {
		t.Fatalf("top-level not truncated by characters")
	}
	if out["list"].([]any)[0] != want || out["m"].(map[string]any)["k"] != want {
		t.Fatalf("nested strings not truncated: %v", out)
	}
	if out["short"] != strings.Repeat("x", 500) {
		t.Fatal("500 chars must be kept whole")
	}
}

func TestRecordFailureDoesNotPanic(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	d := Deps{InWorkspace: func(context.Context, string, func(pgx.Tx) error) error {
		return context.DeadlineExceeded
	}}
	Record(context.Background(), d, Ctx{WorkspaceID: "ws"}, "create_lead", Args{"token": "SUPERSECRET"},
		AuditOutcome{Status: "ok"})
	Record(context.Background(), Deps{}, Ctx{WorkspaceID: "ws"}, "t", nil, AuditOutcome{Status: "denied"}) // no runner
	got := buf.String()
	if !strings.Contains(got, "mcp audit write failed") || !strings.Contains(got, "create_lead") || !strings.Contains(got, "status=ok") {
		t.Fatalf("log = %q", got)
	}
	if strings.Contains(got, "SUPERSECRET") {
		t.Fatal("secret in log")
	}
}

func TestRecordTimeoutBounded(t *testing.T) {
	old := auditTimeout
	auditTimeout = 100 * time.Millisecond
	defer func() { auditTimeout = old }()
	block := make(chan struct{})
	defer close(block)
	// A runner that ignores its context entirely: Record must still return.
	d := Deps{InWorkspace: func(context.Context, string, func(pgx.Tx) error) error { <-block; return nil }}
	start := time.Now()
	Record(context.Background(), d, Ctx{WorkspaceID: "ws"}, "t", nil, AuditOutcome{Status: "error", Error: "x"})
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Record blocked %v", el)
	}
}
