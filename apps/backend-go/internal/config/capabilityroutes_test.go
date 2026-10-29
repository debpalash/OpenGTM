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

func TestTrailingSlashCompatFlag(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if on, err := TrailingSlashCompatFrom(get(nil)); err != nil || on {
		t.Fatalf("default must be off: %v %v", on, err)
	}
	// independent of GO_CAPABILITY_ROUTES_ENABLED
	if on, _ := TrailingSlashCompatFrom(get(map[string]string{"GO_CAPABILITY_ROUTES_ENABLED": "true"})); on {
		t.Error("the capability flag must not switch trailing-slash compat on")
	}
	if on, err := TrailingSlashCompatFrom(get(map[string]string{"GO_TRAILING_SLASH_COMPAT": "true"})); err != nil || !on {
		t.Errorf("true: %v %v", on, err)
	}
	if _, err := TrailingSlashCompatFrom(get(map[string]string{"GO_TRAILING_SLASH_COMPAT": "maybe"})); err == nil {
		t.Error("garbage accepted")
	}
}
