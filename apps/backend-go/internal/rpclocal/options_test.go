package rpclocal

import (
	"context"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

type inputRunner struct{ in []providers.Input }

func (r *inputRunner) RunLocal(_ context.Context, _, _ string, in providers.Input, _ time.Duration) (providers.Output, error) {
	r.in = append(r.in, in)
	return providers.Output{Error: "no_people_found"}, nil
}

// The provider-call input's "_options" object carries the caller's per-call tuning (collect stage
// settings) into Input.Settings; it is not part of the lead.
func TestCallOptionsReachProvider(t *testing.T) {
	r := &inputRunner{}
	c := New(Deps{Local: only("crosslinked"), Runner: r})
	if _, err := c.Call(context.Background(), call("crosslinked", "o1",
		`{"company":"Acme","_options":{"max_people":3,"delay_ms":1500}}`)); err != nil {
		t.Fatal(err)
	}
	if len(r.in) != 1 {
		t.Fatal("runner not called")
	}
	in := r.in[0]
	if _, ok := in.Lead["_options"]; ok || in.Lead["company"] != "Acme" {
		t.Fatalf("lead %v", in.Lead)
	}
	if in.Settings["max_people"] != 3 || in.Settings["delay_ms"] != 1500 {
		t.Fatalf("settings %v", in.Settings)
	}
}
