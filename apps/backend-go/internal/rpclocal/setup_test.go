package rpclocal

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

func TestConfigure(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	t.Cleanup(func() { providers.SetLocalGate(nil) })
	c, err := Configure(Setup{Getenv: env(map[string]string{"SEARXNG_DISABLE": "1"})})
	if err != nil || c != nil {
		t.Fatalf("nothing listed, no worker: %v %v", c, err)
	}
	jobspy, _ := providers.Get("jobspy")
	if providers.InProcess(jobspy) {
		t.Fatal("jobspy in process by default")
	}
	c, err = Configure(Setup{Getenv: env(map[string]string{"GO_LOCAL_PROVIDERS": "jobspy,person_intel_deep"})})
	if err != nil || c == nil || !c.Serves("jobspy") || !c.Serves("person_intel_deep") || c.Serves("crosslinked") {
		t.Fatalf("%v %v", c, err)
	}
	if !providers.InProcess(jobspy) {
		t.Fatal("gate not installed")
	}
	if _, err := Configure(Setup{Getenv: env(map[string]string{"GO_LOCAL_PROVIDERS": "bad name"})}); err == nil {
		t.Fatal("bad flag accepted")
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
