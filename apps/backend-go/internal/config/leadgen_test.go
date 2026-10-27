package config

import "testing"

func TestLeadgenConfigDefaults(t *testing.T) {
	l, err := loadLeadgen(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := LeadgenSettings{CollectQueryMaxChars: 1000, SystemStatsRPCTimeoutSeconds: 3}
	if l != want {
		t.Fatalf("defaults\n got %#v\nwant %#v", l, want)
	}
	if l.GoCollectionRoutesEnabled {
		t.Fatal("rollout flags must default off")
	}
}

func TestLeadgenConfigParseAndReject(t *testing.T) {
	env := map[string]string{"GO_COLLECTION_ROUTES_ENABLED": "true",
		"COLLECT_QUERY_MAX_CHARS": "200", "SYSTEM_STATS_RPC_TIMEOUT_SECONDS": "9"}
	l, err := loadLeadgen(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !l.GoCollectionRoutesEnabled || l.CollectQueryMaxChars != 200 || l.SystemStatsRPCTimeoutSeconds != 9 {
		t.Fatalf("parse: %#v", l)
	}
	for _, kv := range [][2]string{{"GO_COLLECTION_ROUTES_ENABLED", "maybe"}, {"COLLECT_QUERY_MAX_CHARS", "0"}, {"SYSTEM_STATS_RPC_TIMEOUT_SECONDS", "x"}} {
		if _, err := loadLeadgen(func(k string) string {
			if k == kv[0] {
				return kv[1]
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s must be rejected", kv[0], kv[1])
		}
	}
}
