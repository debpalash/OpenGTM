package api

import "testing"

// GO_EXECUTOR_FULL_RUN (plan 15 WP-WBEXEC) reaches runplan.Config; retired at the cutover, so it is on whatever
// the environment says.
func TestExecutorSettingsFullRun(t *testing.T) {
	for _, v := range []string{"", "false", "maybe", "true"} {
		get := func(k string) string {
			if k == "GO_EXECUTOR_FULL_RUN" {
				return v
			}
			return ""
		}
		if !executorSettings(get).FullRun {
			t.Fatalf("GO_EXECUTOR_FULL_RUN=%q must read on", v)
		}
		h := &runRoutes{d: Deps{Getenv: get}, index: newManifestIndexer(func(string) string { return "" })}
		if !h.config(t.Context(), "ws").FullRun {
			t.Fatalf("runplan.Config.FullRun must be on (%q)", v)
		}
	}
}

// Review L1/L2: the API reads a malformed switch the way the worker does (off), whatever the other says.
func TestExecutorSettingsMalformedFlags(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if s := executorSettings(env(map[string]string{"PROVENANCE_TRACKING_ENABLED": "yes"})); !s.Provenance {
		t.Fatalf("%+v", s)
	}
	if s := executorSettings(env(map[string]string{"PROVENANCE_TRACKING_ENABLED": "maybe"})); s.Provenance {
		t.Fatalf("%+v", s)
	}
}
