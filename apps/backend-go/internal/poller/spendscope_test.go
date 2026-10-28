package poller

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
)

type scopeCaller struct{ scopes []provcall.SpendScope }

func (c *scopeCaller) Call(ctx context.Context, _ provcall.Call) (provcall.Result, error) {
	s, _ := provcall.SpendScopeFrom(ctx)
	c.scopes = append(c.scopes, s)
	return provcall.Result{Status: "ok", Output: []byte(`{"success":true,"fields":{}}`)}, nil
}

// Poll provider calls are paid work of the workspace spend ledger: the scope names the watch fire and the source.
func TestPollProviderCallsCarryASpendScope(t *testing.T) {
	cc := &scopeCaller{}
	d := pcDeps(cc)
	if _, err := d.Py.pcCall(context.Background(), pcWatch(), "hiring", 0, "jobspy_signals", map[string]any{"company": "Acme"}, 0); err != nil {
		t.Fatal(err)
	}
	if len(cc.scopes) != 1 || cc.scopes[0].Run != "watch:w-1:fk-1" || cc.scopes[0].Row != "hiring" {
		t.Fatalf("scopes %+v", cc.scopes)
	}
}
