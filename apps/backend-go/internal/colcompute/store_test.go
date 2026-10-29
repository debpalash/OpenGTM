package colcompute

import (
	"encoding/json"
	"math/big"
	"testing"
)

func TestStoredValue(t *testing.T) {
	cases := []struct {
		in   any
		text string
		raw  string
	}{
		{"héllo", "héllo", `"héllo"`},
		{big.NewInt(6), "6", `6`},
		{1.5, "1.5", `1.5`},
		{1e16, "1e+16", `1e+16`},
		{[]any{"a", big.NewInt(1)}, `["a", 1]`, `["a",1]`},
	}
	for _, c := range cases {
		text, raw, err := StoredValue(c.in)
		if err != nil || text != c.text {
			t.Fatalf("%#v: text %q err %v", c.in, text, err)
		}
		b, _ := json.Marshal(raw)
		if string(b) != c.raw {
			t.Fatalf("%#v: raw %s want %s", c.in, b, c.raw)
		}
	}
}
