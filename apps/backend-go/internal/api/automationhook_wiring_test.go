package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F7: every API emitter routes trigger_eval jobs through the go_executor cohort.
func TestAutomationExecutorFollowsCohort(t *testing.T) {
	env := map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "a, b"}
	d := Deps{Getenv: func(k string) string { return env[k] }}
	d.Flags.Automations.GoExecutorTriggerEval = true
	exec := d.automationExecutor()
	if exec("a") != "go" || exec("b") != "go" || exec("c") != "python" || exec("") != "python" {
		t.Fatalf("cohort: a=%s b=%s c=%s", exec("a"), exec("b"), exec("c"))
	}
	d.Flags.Automations.GoExecutorTriggerEval = false
	if d.automationExecutor()("a") != "python" {
		t.Fatal("GO_EXECUTOR_TRIGGER_EVAL off must route to python")
	}
	d.Flags.Automations.GoExecutorTriggerEval = true
	env["GO_EXECUTOR_ENABLED"] = "false"
	if d.automationExecutor()("a") != "python" {
		t.Fatal("GO_EXECUTOR_ENABLED off must route to python")
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
