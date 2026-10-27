package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F7: every API emitter routes trigger_eval jobs to the Go worker (plan 15: the executor flags are retired, so
// neither the cohort nor GO_EXECUTOR_TRIGGER_EVAL / GO_EXECUTOR_ENABLED can send one to Python).
func TestAutomationExecutorFollowsCohort(t *testing.T) {
	env := map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "a, b"}
	d := Deps{Getenv: func(k string) string { return env[k] }}
	d.Flags.Automations.GoExecutorTriggerEval = true
	for _, flag := range []bool{true, false} {
		d.Flags.Automations.GoExecutorTriggerEval = flag
		for _, enabled := range []string{"true", "false"} {
			env["GO_EXECUTOR_ENABLED"] = enabled
			exec := d.automationExecutor()
			for _, ws := range []string{"a", "b", "c", ""} {
				if got := exec(ws); got != "go" {
					t.Errorf("trigger_eval=%v enabled=%s ws=%q: executor %s, want go", flag, enabled, ws, got)
				}
			}
		}
	}
}

// No API file may build the python-only emitter again (automationhook.New).
func TestNoPythonOnlyAutomationHookInAPI(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "automationhook.New(") {
			t.Errorf("%s builds automationhook.New: use d.automationHook()", f)
		}
	}
}
