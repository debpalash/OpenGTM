package destvalidate

import (
	"strings"
	"testing"
)

func TestAttioConfig(t *testing.T) {
	if !IsType("attio") {
		t.Fatal("attio not registered")
	}
	if _, err := ValidateConfig("attio", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateConfig("attio", map[string]any{"api_key": "x"}); err == nil || !strings.Contains(err.Error(), "credentials must use workspace secrets") {
		t.Fatalf("credential key accepted: %v", err)
	}
}
