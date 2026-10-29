package rpclocal

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// GO_LOCAL_PROVIDERS is retired (plan 15 cutover): every provider is served in process whatever the variable
// says, and Configure always returns a caller (there is no Python worker to fall back to).
func TestConfigure(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	t.Cleanup(func() { providers.SetLocalGate(nil) })
	jobspy, _ := providers.Get("jobspy")
	for _, v := range []string{"", "jobspy", "bad name"} {
		providers.SetLocalGate(nil)
		c, err := Configure(Setup{Getenv: env(map[string]string{"GO_LOCAL_PROVIDERS": v, "SEARXNG_DISABLE": "1"})})
		if err != nil || c == nil || !c.Serves("jobspy") || !c.Serves("person_intel_deep") || !c.Serves("crosslinked") {
			t.Fatalf("GO_LOCAL_PROVIDERS=%q: %v %v", v, c, err)
		}
		if !providers.InProcess(jobspy) {
			t.Fatalf("GO_LOCAL_PROVIDERS=%q: gate not installed", v)
		}
	}
}

// M3 wiring: Configure hands the binary's providerrun runner (pool-backed: cache and stats ledger)
// to the caller.
func TestConfigureUsesRunner(t *testing.T) {
	t.Cleanup(func() { providers.SetLocalGate(nil) })
	fr := &fakeRunner{out: providers.Output{Error: "no_company"}}
	c, err := Configure(Setup{Getenv: func(k string) string {
		return map[string]string{"GO_LOCAL_PROVIDERS": "tech_stack", "SEARXNG_DISABLE": "1"}[k]
	}, Runner: fr})
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if _, err := c.Call(t.Context(), call("tech_stack", "c1", `{}`)); err != nil || len(fr.calls) != 1 {
		t.Fatalf("runner not used: %v %v", fr.calls, err)
	}
}
