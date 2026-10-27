package config

import (
	"testing"
	"time"
)

func TestLoadCollect(t *testing.T) {
	s, err := LoadCollect(envOf(map[string]string{}))
	if err != nil || s.GoCollect || s.GoBulkEnrich || s.NativeProbe || s.HealthEnforce || s.SourceReliability {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	if s.RegistrySourceCap != 20 || s.DMBudget != 15 || s.EmailWaterfallBudget != 15 || s.DeliverabilityBudget != 20 ||
		s.SearchTimeout != 25*time.Second {
		t.Fatalf("knob defaults: %+v", s)
	}
	s, err = LoadCollect(envOf(map[string]string{"GO_EXECUTOR_COLLECT": "true", "GO_EXECUTOR_BULK_ENRICH": "1",
		"GO_NATIVE_SOURCE_PROBE": "yes", "REGISTRY_SOURCE_CAP": "-3", "DDG_SEARCH_TIMEOUT": "9"}))
	if err != nil || !s.GoCollect || !s.GoBulkEnrich || !s.NativeProbe || s.RegistrySourceCap != -3 || s.SearchTimeout != 9*time.Second {
		t.Fatalf("set: %+v %v", s, err)
	}
	for _, bad := range []map[string]string{{"GO_EXECUTOR_COLLECT": "maybe"}, {"DM_DISCOVERY_BUDGET": "x"},
		{"DDG_SEARCH_TIMEOUT": "0"}} {
		if _, err := LoadCollect(envOf(bad)); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}
}

func TestCollectExecutor(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		ws   string
		want string
	}{
		{map[string]string{}, "w1", "python"},
		{map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "w1"}, "w1", "python"},
		{map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "w1", "GO_EXECUTOR_COLLECT": "1"}, "w1", "go"},
		{map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "w2", "GO_EXECUTOR_COLLECT": "1"}, "w1", "python"},
		{map[string]string{"GO_EXECUTOR_COLLECT": "1", "GO_EXECUTOR_WORKSPACES": "w1"}, "w1", "python"},
		{map[string]string{"GO_ALL_NATIVE": "true"}, "any", "go"},
	} {
		g := AllNative(envOf(c.env))
		if got := CollectExecutor(g, c.ws); got != c.want {
			t.Errorf("CollectExecutor(%v, %s) = %s want %s", c.env, c.ws, got, c.want)
		}
		if got := BulkEnrichExecutor(g, c.ws); c.env["GO_EXECUTOR_COLLECT"] == "" && got != c.want {
			t.Errorf("BulkEnrichExecutor(%v, %s) = %s want %s", c.env, c.ws, got, c.want)
		}
	}
	g := envOf(map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "w1", "GO_EXECUTOR_BULK_ENRICH": "1"})
	if BulkEnrichExecutor(g, "w1") != "go" || CollectExecutor(g, "w1") != "python" {
		t.Error("bulk_enrich flag is independent of collect")
	}
}
