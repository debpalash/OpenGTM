package copilot

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestFormatVectors(t *testing.T) {
	b, err := os.ReadFile("testdata/format.json")
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	fns := map[string]func(map[string]any) string{
		"people_research":     FormatPeopleResearch,
		"people_verification": FormatPeopleVerification,
		"people_contacts":     FormatPeopleContacts,
		"signal_tracking":     FormatSignalTracking,
		"grounded_draft":      FormatGroundedDraft,
	}
	total := 0
	for name, fn := range fns {
		var cases []struct {
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
		}
		if err := json.Unmarshal(top[name], &cases); err != nil {
			t.Fatal(err)
		}
		for i, c := range cases {
			dec := json.NewDecoder(bytes.NewReader(c.Input))
			dec.UseNumber()
			var in map[string]any
			if err := dec.Decode(&in); err != nil {
				t.Fatal(err)
			}
			if got := fn(in); got != c.Output {
				t.Errorf("%s[%d] input %s\n got %q\nwant %q", name, i, c.Input, got, c.Output)
			}
			total++
		}
	}
	if total != 26 {
		t.Fatalf("ran %d vectors, want 26", total)
	}
}

// Plain Go values (not decoded with UseNumber) must format the same way.
func TestFormatAcceptsGoNumbers(t *testing.T) {
	got := FormatPeopleResearch(map[string]any{
		"company": "A", "function": "f",
		"people": []any{map[string]any{"name": "N", "confidence": 0.5}, map[string]any{"name": "M", "confidence": 1}},
	})
	want := "I found **2 evidence-matched candidate(s)** for **A — f**.\n\n| Person | Public title | Location | Retrieved | Confidence |\n|---|---|---|---|---|\n| N | — | — | — | 50% |\n| M | — | — | — | 100% |\n\nEvery row passed an exact target-company employment pattern and function-term check. Public profile snippets can still be stale, so re-check the linked profile before outreach. No email addresses were inferred."
	if got != want {
		t.Fatalf("got %q", got)
	}
}
