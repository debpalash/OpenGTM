package providerrun_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
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

type noSecrets struct{}

func (noSecrets) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

func TestGatedProviderFollowsLocalGate(t *testing.T) {
	t.Cleanup(func() { providers.SetLocalGate(nil) })
	r := providerrun.New(providerrun.Deps{Secrets: noSecrets{}})
	in := providers.Input{Lead: map[string]any{"company": "x"}, Field: "email"}

	// Not served in process: unavailable (no Python provider RPC since the plan 15 cutover).
	providers.SetLocalGate(nil)
	if _, err := r.Run(context.Background(), "ws1", "providerrun_test_gated", in, 0); !errors.Is(err, providerrun.ErrUnavailable) {
		t.Fatalf("unlisted: %v", err)
	}
	providers.SetLocalGate(func(n string) bool { return n == "providerrun_test_gated" })
	out, err := r.Run(context.Background(), "ws1", "providerrun_test_gated", in, 0)
	if err != nil || out.Value != "go@x.io" || gated.ws != "ws1" {
		t.Fatalf("listed: %+v %v ws=%q", out, err, gated.ws)
	}
}
