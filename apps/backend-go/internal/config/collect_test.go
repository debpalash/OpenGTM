package config

import (
	"testing"
	"time"
)

func TestLoadCollect(t *testing.T) {
	s, err := LoadCollect(envOf(map[string]string{}))
	// The executor switches are retired (WP-CUTOVER): always on.
	if err != nil || !s.GoCollect || !s.GoBulkEnrich || !s.NativeProbe || s.HealthEnforce || s.SourceReliability {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	if s.RegistrySourceCap != 20 || s.DMBudget != 15 || s.EmailWaterfallBudget != 15 || s.DeliverabilityBudget != 20 ||
		s.SearchTimeout != 25*time.Second {
		t.Fatalf("knob defaults: %+v", s)
	}
	s, err = LoadCollect(envOf(map[string]string{"GO_EXECUTOR_COLLECT": "false", "GO_EXECUTOR_BULK_ENRICH": "0",
		"GO_NATIVE_SOURCE_PROBE": "maybe", "REGISTRY_SOURCE_CAP": "-3", "DDG_SEARCH_TIMEOUT": "9"}))
	if err != nil || !s.GoCollect || !s.GoBulkEnrich || !s.NativeProbe || s.RegistrySourceCap != -3 || s.SearchTimeout != 9*time.Second {
		t.Fatalf("set: %+v %v", s, err)
	}
	for _, bad := range []map[string]string{{"DM_DISCOVERY_BUDGET": "x"},
		{"DDG_SEARCH_TIMEOUT": "0"}} {
		if _, err := LoadCollect(envOf(bad)); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}
}
