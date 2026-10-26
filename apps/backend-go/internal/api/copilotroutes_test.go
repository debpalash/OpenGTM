package api

import "testing"

func TestRestoreLineSeparators(t *testing.T) {
	bs := string(rune(92))
	for in, want := range map[string]string{
		`plain`:                            `plain`,
		bs + "u2028":                       " ",
		bs + "u2029x":                      " x",
		bs + bs + "u2028":                  bs + bs + "u2028", // an escaped backslash followed by the text u2028
		bs + bs + bs + "u2028":             bs + bs + " ",
		bs + `"` + bs + "u2028":            bs + `"` + " ",
		bs + "u2027" + bs + "u202":         bs + "u2027" + bs + "u202",
		`a` + bs + "u2028b" + bs + "u2029": "a b ",
	} {
		if got := string(restoreLineSeparators([]byte(in))); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
