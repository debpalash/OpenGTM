package main

import (
	"io"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

func govCfg(ret, pb bool) config.Config {
	return config.Config{Flags: config.Flags{Governance: config.GovernanceSettings{GoExecutorRetention: ret, GoExecutorPlaybooks: pb}}}
}

// Each handler is registered only when its executor flag is on.
func TestGovernanceHandlersFollowFlags(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	cases := []struct {
		ret, pb bool
		want    []string
	}{
		{false, false, nil},
		{true, false, []string{"retention_enforce"}},
		{false, true, []string{"research_playbook_run", "research_playbook_schedule"}},
		{true, true, []string{"retention_enforce", "research_playbook_run", "research_playbook_schedule"}},
	}
	for _, c := range cases {
		got := governanceHandlers(govCfg(c.ret, c.pb), nil, log, nil, governanceOpts{})
		if got == nil || len(got) != len(c.want) {
			t.Fatalf("ret=%v pb=%v: handlers %v, want %v", c.ret, c.pb, got, c.want)
		}
		for _, k := range c.want {
			if got[k] == nil {
				t.Errorf("ret=%v pb=%v: %s not registered", c.ret, c.pb, k)
			}
		}
	}
}

func TestGovernanceJobLimits(t *testing.T) {
	for typ, secs := range map[string]int{"retention_enforce": 1800, "research_playbook_run": 3600, "research_playbook_schedule": 300} {
		d, ok := governanceLimit(typ)
		if !ok || int(d.Seconds()) != secs {
			t.Errorf("%s: %v %v, want %ds", typ, d, ok, secs)
		}
	}
	if _, ok := governanceLimit("send"); ok {
		t.Error("send is not a governance job")
	}
}
