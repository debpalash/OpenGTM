package providerrun

import "testing"

func TestGlobalSettingKeysPerProvider(t *testing.T) {
	want := map[string][]string{
		"reacher":      {"REACHER_URL", "REACHER_ENABLED", "REACHER_TIMEOUT"},
		"tech_stack":   {"TECH_STACK_WEBSITE_FETCH_ENABLED", "TECH_STACK_RESPECT_ROBOTS"},
		"sec_edgar":    {"SEC_EDGAR_USER_AGENT"},
		"mca_registry": {"MCA_RESOURCE_ID"},
	}
	for p, keys := range want {
		got := globalSettingKeys(p)
		if len(got) != len(keys) {
			t.Fatalf("%s: %v want %v", p, got, keys)
		}
		for i := range keys {
			if got[i] != keys[i] {
				t.Fatalf("%s: %v want %v", p, got, keys)
			}
		}
	}
	if got := globalSettingKeys("hunter_io"); len(got) != 0 {
		t.Fatalf("hunter_io needs no settings: %v", got)
	}
}
