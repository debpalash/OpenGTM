package manifest_test

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

func TestNormalizeVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.Normalize) < 30 {
		t.Fatalf("only %d vectors", len(v.Normalize))
	}
	for _, c := range v.Normalize {
		in, _ := dec(t, c.Fields).(map[string]any)
		sameValue(t, "normalize/"+c.Name, manifest.Normalize(in), c.Expected)
	}
}

func TestNormalizeDoesNotMutateInput(t *testing.T) {
	in := map[string]any{"phone": "(415) 555-0132"}
	manifest.Normalize(in)
	if in["phone"] != "(415) 555-0132" {
		t.Fatal("input mutated")
	}
}

func TestInputContextVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.InputCtx) < 25 {
		t.Fatalf("only %d vectors", len(v.InputCtx))
	}
	for _, c := range v.InputCtx {
		sameValue(t, "input_ctx/"+c.Name, manifest.InputContext(c.Lead), c.Expected)
	}
}
