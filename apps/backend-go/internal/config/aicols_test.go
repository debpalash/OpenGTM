package config

import "testing"

func TestLoadAICols(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if a := LoadAICols(env(nil)); a.Native || len(a.Problems) != 0 {
		t.Fatalf("default: %+v", a)
	}
	if a := LoadAICols(env(map[string]string{"GO_NATIVE_AI_COLUMNS": "true"})); !a.Native {
		t.Fatalf("on: %+v", a)
	}
	if a := LoadAICols(env(map[string]string{"GO_NATIVE_AI_COLUMNS": "maybe"})); a.Native || len(a.Problems) != 1 {
		t.Fatalf("malformed: %+v", a)
	}
}
