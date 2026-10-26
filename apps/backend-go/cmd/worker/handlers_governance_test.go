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

// Every governance type always has a handler (a hand-back while its flag is off), so the
// claim never fails a go row as unsupported.
func TestGovernanceHandlersAlwaysRegistered(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	for _, f := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		got := governanceHandlers(govCfg(f[0], f[1]), nil, log, nil, governanceOpts{})
		for _, k := range []string{"retention_enforce", "research_playbook_run", "research_playbook_schedule"} {
			if got[k] == nil {
				t.Errorf("flags %v: %s not registered", f, k)
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
