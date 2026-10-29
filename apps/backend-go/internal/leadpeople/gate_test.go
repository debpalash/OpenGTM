package leadpeople

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// gatedCL is a GO_LOCAL_PROVIDERS-gated people finder that records the workspace on its ctx.
type gatedCL struct {
	fakeCL
	ws []string
}

func (*gatedCL) Name() string { return "crosslinked-gated-test" }
func (*gatedCL) LocalGated()  {}
func (*gatedCL) Enrich(context.Context, *http.Client, func() (string, error), providers.Input) (providers.Output, error) {
	return providers.Output{}, nil
}
func (g *gatedCL) FindPeopleByTitles(ctx context.Context, q PeopleQuery) (PeopleFound, error) {
	g.ws = append(g.ws, providers.WorkspaceFrom(ctx))
	return g.fakeCL.FindPeopleByTitles(ctx, q)
}

// L8: a gated registry finder runs only when GO_LOCAL_PROVIDERS lists it, and its search cache is
// salted with the calling workspace.
func TestResearchRespectsLocalGateAndWorkspace(t *testing.T) {
	g := &gatedCL{fakeCL: fakeCL{found: PeopleFound{SearchesUsed: 1}}}
	providers.RegisterForTest(t, g)
	t.Cleanup(func() { providers.SetLocalGate(nil) })
	s := &Service{CrossLinkedName: "crosslinked-gated-test", Identity: &fakeID{res: map[string]any{"status": "not_found"}}}

	providers.SetLocalGate(nil)
	if _, err := s.ResearchPeopleAtCompany(context.Background(), "Stripe", "Sales", ResearchOptions{Workspace: "ws-a"}); !errors.Is(err, ErrCrossLinkedUnavailable) {
		t.Fatalf("gate off: err = %v, calls %d", err, len(g.calls))
	}
	providers.SetLocalGate(func(n string) bool { return n == "crosslinked-gated-test" })
	if _, err := s.ResearchPeopleAtCompany(context.Background(), "Stripe", "Sales", ResearchOptions{Workspace: "ws-a"}); err != nil {
		t.Fatal(err)
	}
	if len(g.ws) != 1 || g.ws[0] != "ws-a" {
		t.Fatalf("finder ctx workspaces %v, want [ws-a]", g.ws)
	}
}
