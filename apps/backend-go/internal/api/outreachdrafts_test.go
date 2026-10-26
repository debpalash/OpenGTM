package api

import "testing"

func TestDraftJSONKeepsRawLineSeparators(t *testing.T) {
	cases := []struct{ in, empty, want string }{
		{`[{"s": "a b c"}]`, "[]", "[{\"s\":\"a b c\"}]"},
		{`[{"s": "back\\u2028slash"}]`, "[]", `[{"s":"back\\u2028slash"}]`}, // literal backslash + u2028 stays escaped
		{`[{"s": "\\ "}]`, "[]", "[{\"s\":\"\\\\ \"}]"},
		{`null`, "[]", `[]`},
		{`{}`, "[]", `[]`},
		{`[]`, "{}", `{}`},
		{`0`, "{}", `{}`},
		{`""`, "[]", `[]`},
		{`false`, "{}", `{}`},
		{`{"a": 1}`, "{}", `{"a":1}`},
	}
	for _, c := range cases {
		if got := string(pyJSONText(c.in, c.empty)); got != c.want {
			t.Errorf("%s: got %q want %q", c.in, got, c.want)
		}
	}
}
