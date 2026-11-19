package sqllike

import "testing"

func TestEscape(t *testing.T) {
	for _, c := range []struct {
		in, esc, want string
	}{
		{"plain", `\`, "plain"},
		{"50%", `\`, `50\%`},
		{"a_b", `\`, `a\_b`},
		{`a\b`, `\`, `a\\b`},
		{`%_\`, `\`, `\%\_\\`},
		{"a/b%c_d", "/", "a//b/%c/_d"},
		{"", `\`, ""},
	} {
		if got := Escape(c.in, c.esc[0]); got != c.want {
			t.Errorf("Escape(%q, %q) = %q, want %q", c.in, c.esc, got, c.want)
		}
	}
}

func TestContains(t *testing.T) {
	if got := Contains(`100%_x`); got != `%100\%\_x%` {
		t.Fatalf("got %q", got)
	}
	if got := Contains(""); got != "%%" {
		t.Fatalf("empty search must still match everything, got %q", got)
	}
}
