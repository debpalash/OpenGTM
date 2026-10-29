package settings

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func newEnrichment(t *testing.T, st *fakeStore) *EnrichmentProviders {
	t.Helper()
	app, _ := testApp(t, st, nil)
	return &EnrichmentProviders{Reg: mustLoad(t), App: app, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestEnrichmentProviderMaskAndUpdate(t *testing.T) {
	st := newFake()
	e := newEnrichment(t, st)
	ctx := context.Background()

	views, err := e.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != e.Reg.EnrichmentProviders.Len() || views[0].Configured || views[0].APIKeyMasked != "" {
		t.Fatalf("initial %+v", views[0])
	}

	key := "hunter-contract-key-1234567890"
	found, err := e.Update(ctx, "hunter_io", ProviderUpdate{APIKey: &key})
	if err != nil || !found {
		t.Fatalf("update: %v %v", found, err)
	}
	if raw := st.m["HUNTER_API_KEY"]; !strings.HasPrefix(raw, "enc:") || strings.Contains(raw, "hunter-contract") {
		t.Fatalf("stored value must be ciphertext, got %q", raw)
	}
	short := "short-key"
	if _, err := e.Update(ctx, "apollo_io", ProviderUpdate{APIKey: &short}); err != nil {
		t.Fatal(err)
	}
	views, _ = e.List(ctx)
	byID := map[string]EnrichmentView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if h := byID["hunter_io"]; !h.Configured || h.APIKeyMasked != "hunter-c••••7890" || h.Name != "Hunter.io" || h.DocsURL == "" {
		t.Errorf("hunter %+v", h)
	}
	if a := byID["apollo_io"]; !a.Configured || a.APIKeyMasked != "••••••••" {
		t.Errorf("apollo %+v", a)
	}

	// api_key omitted: nothing written.
	before := len(st.m)
	if found, err := e.Update(ctx, "hunter_io", ProviderUpdate{}); !found || err != nil || len(st.m) != before {
		t.Fatal("update without api_key must not write")
	}
	if found, _ := e.Update(ctx, "nope", ProviderUpdate{APIKey: &key}); found {
		t.Fatal("unknown provider found")
	}
}

func TestEnrichmentUndecryptableKeyDoesNotLeak(t *testing.T) {
	st := newFake()
	st.m["HUNTER_API_KEY"] = "enc:v1:gAAAAAB-garbage-token-xyz"
	views, err := newEnrichment(t, st).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !views[0].Configured || views[0].APIKeyMasked != "••••••••" {
		t.Fatalf("%+v", views[0])
	}
}
