package api

import "testing"

// The retired routing flags are not read by the planner: runplan.Build turns them on whatever the environment says,
// so the config the API assembles carries none of them.
func TestRunConfigCarriesNoRetiredFlags(t *testing.T) {
	get := func(k string) string {
		if k == "GO_EXECUTOR_FULL_RUN" || k == "GO_EXECUTOR_ENABLED" {
			return "false"
		}
		return ""
	}
	h := &runRoutes{d: Deps{Getenv: get}, index: newManifestIndexer(func(string) string { return "" })}
	c := h.config(t.Context(), "ws")
	if c.GoExecutorEnabled || c.FullRun || c.NativeProviders || len(c.GoCohort) != 0 || len(c.ExtraColumnTypes) != 0 {
		t.Fatalf("the API must leave the retired flags to runplan: %+v", c)
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
