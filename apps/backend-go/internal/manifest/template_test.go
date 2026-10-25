package manifest_test

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

func TestRenderVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.Render) < 60 {
		t.Fatalf("only %d render vectors", len(v.Render))
	}
	env := func(k string) string { return v.Env[k] }
	for _, c := range v.Render {
		ctx, _ := dec(t, c.Ctx).(map[string]any)
		got := manifest.Render(dec(t, c.Input), ctx, env)
		sameValue(t, "render/"+c.Name, got, c.Expected)
		if s, ok := dec(t, c.Input).(string); ok {
			if rs := manifest.RenderString(s, ctx, env); rs != got {
				t.Errorf("render/%s: RenderString %q != Render %q", c.Name, rs, got)
			}
		}
	}
}

func TestRenderNilEnv(t *testing.T) {
	if got := manifest.RenderString("[${env:X}]", nil, nil); got != "[]" {
		t.Fatalf("got %q", got)
	}
}
