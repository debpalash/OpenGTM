package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAllNativeOffChangesNothing(t *testing.T) {
	for _, m := range []map[string]string{{}, {"GO_ALL_NATIVE": "false"}, {"GO_ALL_NATIVE": "0", "GO_EXECUTOR_ENABLED": "true"}} {
		get := AllNative(envOf(m))
		for _, f := range NativeFlags() {
			if isRetired(f.Key) {
				continue // permanently on (TestRetiredFlagsArePermanentlyOn)
			}
			if got, want := get(f.Key), m[f.Key]; got != want {
				t.Fatalf("%v: %s = %q, want %q", m, f.Key, got, want)
			}
		}
	}
}

func TestAllNativeTurnsEverythingOn(t *testing.T) {
	get := AllNative(envOf(map[string]string{"GO_ALL_NATIVE": "true", "OTHER": "x"}))
	if get("OTHER") != "x" || get("UNSET_NON_FLAG") != "" {
		t.Fatal("non-flag keys must pass through")
	}
	for _, key := range []string{
		"GO_EXECUTOR_ENABLED", "GO_EXECUTOR_FULL_RUN", "GO_EXECUTOR_LEGACY_OUTREACH_ACTIONS", "GO_CAPABILITY_ROUTES_ENABLED",
		"GO_TRAILING_SLASH_COMPAT", "GO_LLM_ROUTES_ENABLED", "GO_COPILOT_NATIVE", "GO_NATIVE_AI_COLUMNS", "GO_NATIVE_SOURCE_PROBE",
		"GO_SCRAPE_ROUTES_ENABLED", "GO_EXECUTOR_SOURCE_WORKBOOK", "GO_SOURCE_PREVIEW_ENABLED", "GO_SCHEDULER_ENABLED",
		"GO_EXECUTOR_COLLECT", "GO_EXECUTOR_BULK_ENRICH", "GO_RUN_ROUTES_ENABLED", "GO_RUN_NATIVE_PROVIDERS", "GO_EXECUTOR_TRIGGER_EVAL",
	} {
		if v := get(key); v != "true" {
			t.Errorf("%s = %q, want true", key, v)
		}
	}
	if get("GO_LOCAL_PROVIDERS") != "all" || get("GO_EXECUTOR_WORKSPACES") != "*" {
		t.Errorf("providers=%q workspaces=%q", get("GO_LOCAL_PROVIDERS"), get("GO_EXECUTOR_WORKSPACES"))
	}
	if get("GO_EXECUTOR_COLUMN_TYPES") == "" || get("GO_DEST_TYPES") == "" {
		t.Error("list flags must get a full default")
	}
	if get("GO_ALL_NATIVE") != "true" {
		t.Error("the switch itself passes through")
	}
}

func TestAllNativeExplicitValueWins(t *testing.T) {
	get := AllNative(envOf(map[string]string{
		"GO_ALL_NATIVE": "yes", "GO_SCHEDULER_ENABLED": "false", "GO_LLM_ROUTES_ENABLED": "0",
	}))
	if get("GO_SCHEDULER_ENABLED") != "false" || get("GO_LLM_ROUTES_ENABLED") != "0" {
		t.Error("explicit false must stay false")
	}
	if get("GO_COPILOT_NATIVE") != "true" {
		t.Error("unset flags still turn on")
	}
}

func TestAllNativeSwitchSpellings(t *testing.T) {
	for v, on := range map[string]bool{"true": true, "1": true, "ON": true, " yes ": true, "": false, "false": false, "no": false, "garbage": false} {
		if got := AllNativeEnabled(envOf(map[string]string{"GO_ALL_NATIVE": v})); got != on {
			t.Errorf("%q: %v", v, got)
		}
	}
}

func TestExistingReadersConsultTheResolver(t *testing.T) {
	get := AllNative(envOf(map[string]string{"GO_ALL_NATIVE": "true", "GO_EXECUTOR_LEGACY_OUTREACH_ACTIONS": "false"})) // retired: stays on
	if v, err := CapabilityRoutesFrom(get); err != nil || !v {
		t.Fatal("capability routes", v, err)
	}
	if v, err := LLMRoutesFrom(get); err != nil || !v {
		t.Fatal("llm routes", v, err)
	}
	if v, err := CopilotNativeFrom(get); err != nil || !v {
		t.Fatal("copilot native", v, err)
	}
	ex := ExecutorCohortFrom(get)
	if !ex.Enabled || !ex.SignalScan || !InCohort(ex.Workspaces, "any-workspace") {
		t.Fatalf("executor %+v", ex)
	}
	a, err := loadAutomations(get)
	if err != nil || !a.GoExecutorTriggerEval || !a.GoExecutorLegacyOutreachActions {
		t.Fatalf("automations %+v %v", a, err)
	}
}

func TestInCohort(t *testing.T) {
	if !InCohort(map[string]bool{"*": true}, "x") || !InCohort(map[string]bool{"x": true}, "x") || InCohort(map[string]bool{"y": true}, "x") || InCohort(nil, "x") {
		t.Fatal("cohort")
	}
}

// Every GO_* flag in non-test sources must be in the registry or listed here as not a nativeness flag, so a new
// flag cannot be added without the master switch learning about it.
func TestRegistryCoversEverySourceFlag(t *testing.T) {
	notFlags := map[string]bool{
		"GO_ALL_NATIVE": true, "GO_AI_BATCH_ENABLED": true, "GO_ENABLED": true, "GO_EXECUTOR_": true, "GO_COLUMN_TYPES": true,
		"GO_SCHEMA_REVISION": true, "GO_TOOLS": true,
	}
	reg := map[string]bool{}
	for _, f := range NativeFlags() {
		reg[f.Key] = true
	}
	re := regexp.MustCompile(`"(GO_[A-Z0-9_]+)"`)
	root := filepath.Join("..", "..")
	var missing []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			k := m[1]
			if reg[k] || notFlags[k] || strings.HasPrefix(k, "GO_WORKER_") {
				continue
			}
			missing = append(missing, k+" ("+p+")")
		}
		return nil
	})
	if len(missing) > 0 {
		t.Fatalf("GO_* flags read in source but absent from config.NativeFlags (add an entry in allnative.go, or list as non-flag): %v", missing)
	}
}
