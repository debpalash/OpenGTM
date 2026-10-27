package collect

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
)

type capCaller struct{ calls []provcall.Call }

func (c *capCaller) Call(_ context.Context, call provcall.Call) (provcall.Result, error) {
	c.calls = append(c.calls, call)
	return provcall.Result{Status: "ok", Output: json.RawMessage(`{"success":false,"fields":{}}`)}, nil
}

// The provider stages use the job's settings, as JobRunner constructed them:
// CrossLinkedProvider(max_people=3, delay=1.5), JobSpySignalProvider(max_jobs=3, delay=1.5),
// MailScoutVerifyProvider(timeout=5, max_verify=2) — not the registry defaults.
func TestProviderStagesUseJobSettings(t *testing.T) {
	cc := &capCaller{}
	e := LiveEnrich{Caller: cc}
	want := map[string]map[string]any{
		"crosslinked": {"max_people": 3.0, "delay_ms": 1500.0},
		"jobspy":      {"max_jobs": 3.0, "delay_ms": 1500.0},
		"mailscout":   {"max_verify": 2.0, "timeout_ms": 5000.0},
	}
	for name, opts := range want {
		cc.calls = nil
		if _, err := e.Provider(context.Background(), "ws1", name, NewLead(Lead{Company: "Acme"})); err != nil {
			t.Fatal(err)
		}
		var in map[string]any
		_ = json.Unmarshal(cc.calls[0].Input, &in)
		got, _ := in["_options"].(map[string]any)
		for k, v := range opts {
			if got[k] != v {
				t.Errorf("%s option %s = %v, want %v (input %s)", name, k, got[k], v, cc.calls[0].Input)
			}
		}
		if in["company"] != "Acme" {
			t.Errorf("%s: lead fields lost: %s", name, cc.calls[0].Input)
		}
	}
}
