package api

import "testing"

// GO_EXECUTOR_FULL_RUN (plan 15 WP-WBEXEC) reaches runplan.Config; off by default.
func TestExecutorSettingsFullRun(t *testing.T) {
	if executorSettings(func(string) string { return "" }).FullRun {
		t.Fatal("GO_EXECUTOR_FULL_RUN must default off")
	}
	on := executorSettings(func(k string) string {
		if k == "GO_EXECUTOR_FULL_RUN" {
			return "true"
		}
		return ""
	})
	if !on.FullRun {
		t.Fatal("GO_EXECUTOR_FULL_RUN=true must be read")
	}
	h := &runRoutes{d: Deps{Getenv: func(k string) string {
		if k == "GO_EXECUTOR_FULL_RUN" {
			return "1"
		}
		return ""
	}}, index: newManifestIndexer(func(string) string { return "" })}
	if !h.config(t.Context(), "ws").FullRun {
		t.Fatal("runplan.Config.FullRun must follow the flag")
	}
}

// Review L1/L2: the API reads a malformed switch the way the worker does (off), whatever the other says.
func TestExecutorSettingsMalformedFlags(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if s := executorSettings(env(map[string]string{"GO_EXECUTOR_FULL_RUN": "maybe", "PROVENANCE_TRACKING_ENABLED": "yes"})); s.FullRun || !s.Provenance {
		t.Fatalf("%+v", s)
	}
	if s := executorSettings(env(map[string]string{"GO_EXECUTOR_FULL_RUN": "on", "PROVENANCE_TRACKING_ENABLED": "maybe"})); !s.FullRun || s.Provenance {
		t.Fatalf("%+v", s)
	}
}
