package emailverify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

type fakePy struct {
	res  pyworker.ProviderResult
	err  error
	last pyworker.ProviderCall
}

func (f *fakePy) Call(_ context.Context, c pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	f.last = c
	return f.res, f.err
}

func TestWebsiteFinderPicksFirstUsableEmail(t *testing.T) {
	py := &fakePy{res: pyworker.ProviderResult{Status: "ok",
		Output: json.RawMessage(`{"success":true,"fields":{"email":"logo@2x.png","secondary_emails":"noreply@acme.com|real@acme.com"}}`)}}
	f := &WebsiteFinder{Py: py}
	email, _, err := f.Find(context.Background(), "ws1", map[string]any{"website": "acme.com"}, "acme.com")
	if err != nil || email != "real@acme.com" {
		t.Fatalf("email=%q err=%v", email, err)
	}
	if py.last.Provider != WebsiteProvider || py.last.WorkspaceID != "ws1" || py.last.CallID == "" {
		t.Fatalf("call: %+v", py.last)
	}
	// no website: nothing to call
	py.last = pyworker.ProviderCall{}
	if e, _, err := f.Find(context.Background(), "ws1", map[string]any{}, ""); e != "" || err != nil || py.last.Provider != "" {
		t.Fatalf("no website must not call the worker: %q %v %+v", e, err, py.last)
	}
	// worker error becomes a recorded step error
	py.res = pyworker.ProviderResult{Status: "error", Error: "provider worker expired"}
	if _, _, err := f.Find(context.Background(), "ws1", map[string]any{"website": "acme.com"}, ""); err == nil || err.Error() != "provider worker expired" {
		t.Fatalf("err=%v", err)
	}
	if (&WebsiteFinder{}).Available(context.Background(), "ws1") {
		t.Fatal("without a Python worker the website step is skipped")
	}
}

func TestProviderFinderGatesOnCredentials(t *testing.T) {
	calls := 0
	keys := func(_ context.Context, ws, env string) (string, error) {
		calls++
		if ws == "ws-with-keys" {
			return "k-" + env, nil
		}
		return "", nil
	}
	hunter := &ProviderFinder{Label: "hunter", Provider: "hunter_io", Keys: keys}
	snov := &ProviderFinder{Label: "snovio", Provider: "snovio", Keys: keys}
	ctx := context.Background()
	if hunter.Available(ctx, "ws-none") || snov.Available(ctx, "ws-none") {
		t.Fatal("no credentials: unavailable")
	}
	if !hunter.Available(ctx, "ws-with-keys") || !snov.Available(ctx, "ws-with-keys") {
		t.Fatal("credentials present: available")
	}
	_, _, err := hunter.Find(ctx, "ws-none", map[string]any{"website": "acme.com"}, "acme.com")
	if err == nil || err.Error() != "No Hunter.io API key configured" {
		t.Fatalf("err=%v", err)
	}
	if _, _, err := (&ProviderFinder{Label: "x", Provider: "nope", Keys: keys}).Find(ctx, "w", nil, ""); err == nil {
		t.Fatal("unknown provider must error")
	}
}

func TestReacherVerifierMapsProviderResult(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"is_reachable":"safe","smtp":{"is_catch_all":false},"misc":{"is_disposable":false,"is_role_account":false}}`))
	}))
	defer srv.Close()
	enabled := false
	cfg := LiveConfig{
		HTTP: srv.Client(),
		Keys: func(context.Context, string, string) (string, error) { return "reacher-secret", nil },
		ReacherSettings: func(context.Context, string) map[string]any {
			return map[string]any{"REACHER_ENABLED": enabled, "REACHER_URL": srv.URL}
		},
	}
	d := NewLive(cfg)
	ctx := context.Background()
	if got := bootstrap(ctx, d, "ws1"); len(got) != 1 || got[0].Name() != "smtp" {
		t.Fatalf("Reacher off: chain must be SMTP only, got %d verifiers", len(got))
	}
	enabled = true
	chain := bootstrap(ctx, d, "ws1")
	if len(chain) != 2 || chain[0].Name() != "reacher" {
		t.Fatalf("Reacher on: it must lead the chain, got %d", len(chain))
	}
	res, err := chain[0].Verify(ctx, "a@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "valid" || res.Source != "reacher" || res.Confidence != 0.95 || res.Detail != "safe" || res.Email != "a@acme.test" {
		t.Fatalf("result: %+v", res)
	}
	if gotAuth != "Bearer reacher-secret" {
		t.Fatalf("auth header %q", gotAuth)
	}
	// the secret must never appear in a marshalled result
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "reacher-secret") {
		t.Fatal("secret leaked into result")
	}
}
