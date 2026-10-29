package config

import "testing"

func TestCopilotNativeFrom(t *testing.T) {
	for v, want := range map[string]bool{"": false, "false": false, "0": false, "true": true, "1": true, "yes": true} {
		got, err := CopilotNativeFrom(func(k string) string {
			if k == "GO_COPILOT_NATIVE" {
				return v
			}
			return ""
		})
		if err != nil || got != want {
			t.Errorf("%q: got %v err %v, want %v", v, got, err, want)
		}
	}
	if _, err := CopilotNativeFrom(func(string) string { return "maybe" }); err == nil {
		t.Error("a non-boolean value must be an error")
	}
}
