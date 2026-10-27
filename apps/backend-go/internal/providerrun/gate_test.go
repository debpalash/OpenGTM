package providerrun_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

// gatedProv is a GO_LOCAL_PROVIDERS-gated provider (like providers/scrape).
type gatedProv struct{ ws string }

func (*gatedProv) Name() string { return "providerrun_test_gated" }
func (*gatedProv) LocalGated()  {}
func (g *gatedProv) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	g.ws = providers.WorkspaceFrom(ctx)
	x := providers.NewObj()
	x.Set("email", "go@x.io")
	return providers.Output{Value: "go@x.io", Extra: x}, nil
}

var gated = &gatedProv{}

func init() { providers.Register(gated) }

type pyStub struct{ n int }

func (p *pyStub) Call(context.Context, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	p.n++
	return pyworker.ProviderResult{Status: "ok", Output: json.RawMessage(`{"success":true,"fields":{"email":"py@x.io"},"confidence":0.5}`)}, nil
}

type noSecrets struct{}

func (noSecrets) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

func TestGatedProviderFollowsLocalGate(t *testing.T) {
	t.Cleanup(func() { providers.SetLocalGate(nil) })
	py := &pyStub{}
	r := providerrun.New(providerrun.Deps{Secrets: noSecrets{}, Py: py})
	in := providers.Input{Lead: map[string]any{"company": "x"}, Field: "email"}

	providers.SetLocalGate(nil)
	out, err := r.Run(context.Background(), "ws1", "providerrun_test_gated", in, 0)
	if err != nil || out.Value != "py@x.io" || py.n != 1 {
		t.Fatalf("unlisted: %+v %v %d", out, err, py.n)
	}
	providers.SetLocalGate(func(n string) bool { return n == "providerrun_test_gated" })
	out, err = r.Run(context.Background(), "ws1", "providerrun_test_gated", in, 0)
	if err != nil || out.Value != "go@x.io" || py.n != 1 || gated.ws != "ws1" {
		t.Fatalf("listed: %+v %v %d ws=%q", out, err, py.n, gated.ws)
	}
}
