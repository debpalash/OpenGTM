package config

import "testing"

func TestLLMRoutesFrom(t *testing.T) {
	for in, want := range map[string]bool{"": false, "true": true, "1": true, "off": false} {
		got, err := LLMRoutesFrom(func(string) string { return in })
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := LLMRoutesFrom(func(string) string { return "maybe" }); err == nil {
		t.Error("invalid value must be an error")
	}
}
