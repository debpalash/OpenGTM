package config

import (
	"os"
	"regexp"
	"testing"
)

// Keeps the Go and Python schema-revision constants in lockstep. Skips when the
// legacy Python tree is not present.
func TestRequiredSchemaRevisionMatchesPython(t *testing.T) {
	b, err := os.ReadFile("../../../../legacy/python/apps/api/services/go_executor.py")
	if err != nil {
		t.Skipf("python go_executor.py not available: %v", err)
	}
	m := regexp.MustCompile(`(?m)^REQUIRED_GO_SCHEMA_REVISION\s*=\s*"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("REQUIRED_GO_SCHEMA_REVISION not found in go_executor.py")
	}
	if got := string(m[1]); got != RequiredGoSchemaRevision {
		t.Fatalf("python revision %q != go RequiredGoSchemaRevision %q", got, RequiredGoSchemaRevision)
	}
}
