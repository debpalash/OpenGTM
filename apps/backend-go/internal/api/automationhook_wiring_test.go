package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every API emitter routes trigger_eval jobs to the Go worker (plan 15: the executor flags are retired).
func TestAutomationExecutorIsGo(t *testing.T) {
	for _, ws := range []string{"a", "b", "c", ""} {
		if got := goExecutor(ws); got != "go" {
			t.Errorf("ws=%q: executor %s, want go", ws, got)
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
