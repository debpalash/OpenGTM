package copilot

import (
	"encoding/json"
	"testing"
)

func TestParseToolCallsNormalisesArguments(t *testing.T) {
	raw := json.RawMessage(`[
	 {"id":"a","type":"function","function":{"name":"search_leads","arguments":""}},
	 {"id":"b","type":"function","function":{"name":"search_leads","arguments":"{broken"}},
	 {"id":"c","type":"function","function":{"name":"search_leads","arguments":"[1, 2]"}},
	 {"id":"d","type":"function","function":{"name":"update_lead_status","arguments":"{\"lead_id\": 1.0, \"s\": \"é\"}"}},
	 {"id":"e","type":"function","function":{"name":"never_heard_of_it","arguments":"{}"}}]`)
	calls, err := parseToolCalls(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 {
		t.Fatalf("calls %d", len(calls))
	}
	for i, wantArgs := range []string{`{}`, `{}`, `{}`, `{"lead_id": 1.0, "s": "\u00e9"}`, `{}`} {
		if got := string(calls[i].ArgsJSON); got != wantArgs {
			t.Errorf("call %d args %q want %q", i, got, wantArgs)
		}
	}
	// the model sees "{}" for an empty arguments string, anything else unchanged
	if a := calls[0].fnArguments(); a != "{}" {
		t.Errorf("empty arguments -> %q", a)
	}
	if a := calls[1].fnArguments(); a != "{broken" {
		t.Errorf("invalid arguments must stay as sent, got %q", a)
	}
	wantDanger := []bool{false, false, false, true, true}
	for i, c := range calls {
		if c.Dangerous != wantDanger[i] {
			t.Errorf("call %d (%s) dangerous=%v", i, c.Name, c.Dangerous)
		}
	}
	if calls[3].Args["s"] != "é" {
		t.Errorf("args %v", calls[3].Args)
	}
}

func TestParseToolCallsRejectsMalformed(t *testing.T) {
	for _, raw := range []string{`{}`, `[1]`, `[{"id":"a"}]`, `[{"id":"a","function":{"arguments":"{}"}}]`, `[{"id":"a","function":{"name":"x","arguments":5}}]`, `null`} {
		if _, err := parseToolCalls(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: want error", raw)
		}
	}
}

func TestDedupKeyIgnoresKeyOrderButNotNumberKind(t *testing.T) {
	a, _ := parseToolCalls(json.RawMessage(`[{"id":"1","function":{"name":"n","arguments":"{\"a\": 1, \"b\": [1, 2]}"}}]`))
	b, _ := parseToolCalls(json.RawMessage(`[{"id":"2","function":{"name":"n","arguments":"{\"b\": [1, 2], \"a\": 1}"}}]`))
	c, _ := parseToolCalls(json.RawMessage(`[{"id":"3","function":{"name":"n","arguments":"{\"a\": 1.0, \"b\": [1, 2]}"}}]`))
	d, _ := parseToolCalls(json.RawMessage(`[{"id":"4","function":{"name":"m","arguments":"{\"a\": 1, \"b\": [1, 2]}"}}]`))
	if a[0].dedupKey() != b[0].dedupKey() {
		t.Errorf("key order must not matter: %q vs %q", a[0].dedupKey(), b[0].dedupKey())
	}
	if a[0].dedupKey() == c[0].dedupKey() {
		t.Errorf("1 and 1.0 are different Python json.dumps values")
	}
	if a[0].dedupKey() == d[0].dedupKey() {
		t.Errorf("tool name is part of the key")
	}
}

func TestToolResultFrameValue(t *testing.T) {
	for in, want := range map[string]string{
		`{"a": 1}`:          `{"a": 1}`,
		`  [1,2] `:          `[1, 2]`,
		`"s"`:               `"s"`,
		`plain`:             `{"raw": "plain"}`,
		``:                  `{"raw": ""}`,
		`{"a": NaN}`:        `{"raw": "{\"a\": NaN}"}`,
		"{\"a\": \"\xff\"}": `{"a": "\ufffd"}`,
	} {
		if got := string(toolResultValue(in)); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}
