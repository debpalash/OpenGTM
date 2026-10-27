package config

import "testing"

// GO_NATIVE_AI_COLUMNS is retired (WP-CUTOVER): AI columns always run in process, whatever the value.
func TestLoadAICols(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, m := range []map[string]string{nil, {"GO_NATIVE_AI_COLUMNS": "false"}, {"GO_NATIVE_AI_COLUMNS": "maybe"}} {
		if a := LoadAICols(env(m)); !a.Native || len(a.Problems) != 0 {
			t.Fatalf("%v: %+v", m, a)
		}
	}
}
