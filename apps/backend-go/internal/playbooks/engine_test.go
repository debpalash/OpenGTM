package playbooks

import (
	"encoding/json"
	"strings"
	"testing"
)

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
