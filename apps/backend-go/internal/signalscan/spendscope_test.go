package signalscan

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
)

type scopeCaller struct{ scopes []provcall.SpendScope }

func (c *scopeCaller) Call(ctx context.Context, _ provcall.Call) (provcall.Result, error) {
	s, _ := provcall.SpendScopeFrom(ctx)
	c.scopes = append(c.scopes, s)
	return provcall.Result{Status: "ok"}, nil
}

// The hiring detector is paid work of the workspace spend ledger: each lead's call carries a scope of the scan
// and the lead, so a paid hiring provider reserves before it is dispatched.
func TestDetectHiringCarriesASpendScope(t *testing.T) {
	cc := &scopeCaller{}
	d := Deps{Py: cc}
	leads := []Lead{{ID: 7, Company: "Acme"}, {ID: 8, Company: "Beta"}}
	detectHiring(context.Background(), d, "ws-alpha", leads, 5)
	if len(cc.scopes) != 2 {
		t.Fatalf("calls %d", len(cc.scopes))
	}
	if cc.scopes[0].Row != "lead:7" || cc.scopes[1].Row != "lead:8" || cc.scopes[0].Run == "" || cc.scopes[0].Run != cc.scopes[1].Run {
		t.Fatalf("scopes %+v", cc.scopes)
	}
}
