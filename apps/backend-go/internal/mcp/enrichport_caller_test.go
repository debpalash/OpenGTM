package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

type callerStub struct{ got pyworker.ProviderCall }

func (c *callerStub) Call(_ context.Context, call pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	c.got = call
	return pyworker.ProviderResult{Status: "ok", Output: json.RawMessage(`{"fields":{"hiring":true}}`)}, nil
}

// The enrich port takes any provider caller (the rpclocal composite in production).
func TestPyEnrichPortUsesProviderCaller(t *testing.T) {
	st := &callerStub{}
	p := &PyEnrichPort{Worker: st}
	raw, err := p.Py(context.Background(), "ws1", "mcp.get_hiring_signals", map[string]any{"company": "Acme"})
	if err != nil || string(raw) != `{"fields":{"hiring":true}}` || st.got.Provider != "mcp.get_hiring_signals" || st.got.WorkspaceID != "ws1" {
		t.Fatalf("%s %v %+v", raw, err, st.got)
	}
	if _, err := (&PyEnrichPort{}).Py(context.Background(), "ws1", "x", nil); !errors.Is(err, pyworker.ErrUnavailable) {
		t.Fatalf("nil worker: %v", err)
	}
}
