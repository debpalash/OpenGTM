package config

import "testing"

func TestCapabilityRoutesFlag(t *testing.T) {
	get := func(v string) func(string) string {
		return func(k string) string {
			if k == "GO_CAPABILITY_ROUTES_ENABLED" {
				return v
			}
			return ""
		}
	}
	if on, err := CapabilityRoutesFrom(get("")); err != nil || on {
		t.Fatalf("default must be off (Python path): %v %v", on, err)
	}
	for _, v := range []string{"true", "1", "yes", " ON "} {
		if on, err := CapabilityRoutesFrom(get(v)); err != nil || !on {
			t.Errorf("%q: %v %v", v, on, err)
		}
	}
	if on, err := CapabilityRoutesFrom(get("false")); err != nil || on {
		t.Errorf("false: %v %v", on, err)
	}
	if _, err := CapabilityRoutesFrom(get("maybe")); err == nil {
		t.Error("garbage accepted")
	}
}
