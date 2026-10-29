package providerrun_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// Per-call tuning options (the collect stages' max_people / max_jobs / max_verify / delay_ms) reach
// the provider; operator settings still come only from global config and cannot be supplied by a caller.
func TestRunKeepsCallOptions(t *testing.T) {
	var got map[string]any
	name := "providerrun_test_opts"
	providers.RegisterForTest(t, &optsProv{name: name, got: &got})
	r := providerrun.New(providerrun.Deps{Secrets: noSecrets{}})
	in := providers.Input{Lead: map[string]any{"company": "x"}, Field: "email",
		Settings: map[string]any{"max_people": 3, "delay_ms": 1500, "REACHER_URL": "http://169.254.169.254"}}
	if _, err := r.RunLocal(context.Background(), "ws1", name, in, 0); err != nil {
		t.Fatal(err)
	}
	if got["max_people"] != 3 || got["delay_ms"] != 1500 {
		t.Fatalf("call options dropped: %v", got)
	}
	if _, ok := got["REACHER_URL"]; ok {
		t.Fatalf("a caller-supplied operator setting reached the provider: %v", got)
	}
}

type optsProv struct {
	name string
	got  *map[string]any
}

func (o *optsProv) Name() string { return o.name }
func (o *optsProv) Enrich(_ context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	*o.got = in.Settings
	x := providers.NewObj()
	x.Set("email", "a@b.io")
	return providers.Output{Value: "a@b.io", Extra: x}, nil
}
