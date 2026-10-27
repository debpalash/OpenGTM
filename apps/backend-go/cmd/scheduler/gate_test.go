package main

import (
	"errors"
	"strings"
	"testing"
)

// M3: the Go scheduler only runs with GO_SCHEDULER_ENABLED=true (WP-RETIRE-INFRA enables it together with
// removing the Python scheduler, so two reconcilers never coexist).
func TestGateDefaultsOff(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	for _, v := range []string{"", "false", "0", "off"} {
		err := gate(env(v))
		if !errors.Is(err, errDisabled) || !strings.Contains(err.Error(), "GO_SCHEDULER_ENABLED") {
			t.Errorf("%q: err = %v, want errDisabled naming the flag", v, err)
		}
	}
	for _, v := range []string{"true", "1", "TRUE", " yes "} {
		if err := gate(env(v)); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	if err := gate(env("maybe")); err == nil || errors.Is(err, errDisabled) {
		t.Errorf("an invalid value is a config error, got %v", err)
	}
}

func TestStrictRoleDefaultsTrue(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	for v, want := range map[string]bool{"": true, "true": true, "garbage": true, "false": false, " OFF ": false, "0": false} {
		if got := strictRole(env(v)); got != want {
			t.Errorf("%q: %v, want %v", v, got, want)
		}
	}
}
