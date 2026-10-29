package config

import "testing"

// Plan 15 WP-CUTOVER: Python is gone, so the job-executor flags are permanently on. An explicit
// false (or nothing at all, with GO_ALL_NATIVE off) must never route a job to Python again.
func TestRetiredFlagsArePermanentlyOn(t *testing.T) {
	off := map[string]string{"GO_ALL_NATIVE": "false"}
	for _, f := range RetiredFlags() {
		off[f.Key] = "false"
	}
	for _, env := range []map[string]string{{}, off} {
		get := AllNative(envOf(env))
		for _, f := range RetiredFlags() {
			if got := get(f.Key); got != f.On {
				t.Errorf("%v: %s = %q, want the permanent %q", env, f.Key, got, f.On)
			}
		}
	}
	for _, key := range []string{
		"GO_EXECUTOR_ENABLED", "GO_EXECUTOR_WORKSPACES", "GO_EXECUTOR_COLLECT", "GO_EXECUTOR_BULK_ENRICH",
		"GO_EXECUTOR_SOURCE_HEALTH", "GO_EXECUTOR_SIGNAL_SCAN", "GO_EXECUTOR_TRIGGER_EVAL",
		"GO_EXECUTOR_WATCH_POLL", "GO_EXECUTOR_FULL_RUN", "GO_EXECUTOR_OUTREACH_SEND", "GO_EXECUTOR_OUTREACH_INBOUND",
		"GO_EXECUTOR_PLAYBOOKS", "GO_EXECUTOR_RETENTION", "GO_EXECUTOR_AUDIENCE_REFRESH",
		"GO_EXECUTOR_COLUMN_TYPES", "GO_RUN_NATIVE_PROVIDERS", "GO_LOCAL_PROVIDERS",
		"GO_NATIVE_AI_COLUMNS", "GO_NATIVE_SOURCE_PROBE", "GO_DEST_TYPES",
	} {
		if !isRetired(key) {
			t.Errorf("%s is not retired", key)
		}
	}
}

func TestExecutorResolversAlwaysNameGo(t *testing.T) {
	raw := envOf(map[string]string{"GO_EXECUTOR_ENABLED": "false", "GO_EXECUTOR_COLLECT": "false"})
	ex := ExecutorCohortFrom(raw)
	if !ex.Enabled || !ex.SignalScan || !ex.SourceHealth || !InCohort(ex.Workspaces, "any") {
		t.Fatalf("cohort %+v", ex)
	}
	out, err := ex.OutreachExecutor(OutreachSettings{}, "send", "ws")
	if err != nil || out != "go" {
		t.Fatalf("outreach %q %v", out, err)
	}
	for name, got := range map[string]string{
		"refresh":         ex.RefreshExecutor("ws"),
		"signal_scan":     ex.SignalScanExecutor(),
		"source_health":   ex.SourceHealthExecutor(),
		"collect":         CollectExecutor(raw, "ws"),
		"bulk_enrich":     BulkEnrichExecutor(raw, "ws"),
		"source_workbook": SourceWorkbookExecutor(raw, "ws"),
	} {
		if got != "go" {
			t.Errorf("%s executor = %q, want go", name, got)
		}
	}
	if !LoadWbExec(raw).FullRun || !LoadAICols(raw).Native {
		t.Error("the executor switches of the per-area settings must read as on")
	}
}

// Final correctness I4: the route, scheduler and copilot switches are retired too. Without GO_ALL_NATIVE (make run,
// a bare binary) and with a leftover explicit false from the canary period they still read as on.
func TestRouteSchedulerCopilotFlagsAreRetired(t *testing.T) {
	keys := []string{
		"GO_AUDIENCE_ROUTES_ENABLED", "GO_AUTOMATION_ROUTES_ENABLED", "GO_CAPABILITY_ROUTES_ENABLED",
		"GO_COLLECTION_ROUTES_ENABLED", "GO_CONNECTOR_ROUTES_ENABLED", "GO_DEST_ROUTES_ENABLED",
		"GO_GOVERNANCE_ROUTES_ENABLED", "GO_LLM_ROUTES_ENABLED", "GO_OUTREACH_ROUTES_ENABLED",
		"GO_PLAYBOOK_ROUTES_ENABLED", "GO_RUN_ROUTES_ENABLED", "GO_SCRAPE_ROUTES_ENABLED",
		"GO_SOURCE_PREVIEW_ENABLED", "GO_SCHEDULER_ENABLED", "GO_COPILOT_NATIVE", "COPILOT_GO_ENABLED",
	}
	for _, env := range []map[string]string{{}, {"GO_SCHEDULER_ENABLED": "false", "GO_LLM_ROUTES_ENABLED": "0", "COPILOT_GO_ENABLED": "false"}} {
		get := AllNative(envOf(env))
		for _, k := range keys {
			if got := get(k); got != "true" {
				t.Errorf("%v: %s = %q, want true", env, k, got)
			}
		}
	}
	// True opt-ins keep their own defaults.
	get := AllNative(envOf(nil))
	for _, k := range []string{"GO_TRAILING_SLASH_COMPAT", "GO_AI_BATCH_ENABLED"} {
		if get(k) != "" {
			t.Errorf("%s must stay an opt-in, got %q", k, get(k))
		}
	}
}

func TestIgnoredRetiredFlags(t *testing.T) {
	got := IgnoredRetiredFlags(envOf(map[string]string{
		"GO_SCHEDULER_ENABLED": "false", "GO_EXECUTOR_COLLECT": " 0 ", "GO_LLM_ROUTES_ENABLED": "true", "GO_TRAILING_SLASH_COMPAT": "false",
	}))
	want := []string{"GO_EXECUTOR_COLLECT", "GO_SCHEDULER_ENABLED"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v want %v", got, want)
	}
}
