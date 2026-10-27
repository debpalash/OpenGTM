package rpclocal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	sec "github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

const (
	wsSession  = "AQEDAR-ws-a-session-SECRET-777777"
	opSession  = "AQEDAR-operator-env-session-SECRET-999"
	jsessionWS = "ajax:ws-a-jsession-SECRET-55555"
)

// sourcedSecrets answers per (ws, key) with the resolver's source labels.
type sourcedSecrets struct{}

func (sourcedSecrets) Get(_ context.Context, ws, key string) (string, string, error) {
	switch {
	case ws == "ws-a" && key == "LINKEDIN_LI_AT_COOKIE":
		return wsSession, sec.SourceWorkspace, nil
	case ws == "ws-a" && key == "LINKEDIN_JSESSIONID":
		return jsessionWS, sec.SourceWorkspace, nil
	case key == "LINKEDIN_LI_AT_COOKIE": // the operator env value every other workspace falls back to
		return opSession, sec.SourceGlobal, nil
	}
	return "", sec.SourceDefault, nil
}

// L7 + credential rule: staffspy sends only the calling workspace's own LinkedIn session to the
// browser service, never the operator env one; the session never appears in any output, error or log.
func TestStaffSpyWorkspaceSessionOnly(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		// A hostile / buggy service echoes the whole request back.
		_ = json.NewEncoder(w).Encode(map[string]any{"company_name": "Acme", "staff": []any{
			map[string]any{"name": "Jane " + string(b), "title": "CEO", "linkedin": "https://l/j", "location": ""}},
			"error": nil})
	}))
	defer srv.Close()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	env := map[string]string{"GO_LOCAL_PROVIDERS": "staffspy", "STAFFSPY_ENABLED": "1", "SEARXNG_DISABLE": "1",
		"BROWSER_SERVICE_URL": srv.URL, "BROWSER_SERVICE_TOKEN": "tok-123456"}
	t.Cleanup(func() { providers.SetLocalGate(nil) })
	c, err := Configure(Setup{Getenv: func(k string) string { return env[k] }, Secrets: sourcedSecrets{}, Log: log})
	if err != nil || c == nil {
		t.Fatalf("%v %v", c, err)
	}
	run := func(ws, id string) pyworker.ProviderResult {
		res, err := c.Call(context.Background(), pyworker.ProviderCall{CallID: id, Provider: "staffspy", WorkspaceID: ws,
			Input: json.RawMessage(`{"company":"Acme"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// Workspace b has no session of its own: the operator env value is never used.
	res := run("ws-b", "b1")
	if res.Status != "error" || !strings.Contains(res.Error, "staffspy_unavailable") {
		t.Fatalf("ws-b: %+v", res)
	}
	mu.Lock()
	if len(bodies) != 0 {
		t.Fatalf("browser service called without a workspace session: %v", bodies)
	}
	mu.Unlock()
	// Workspace a's own session goes to the service for this request only.
	res = run("ws-a", "a1")
	mu.Lock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], wsSession) || strings.Contains(bodies[0], opSession) {
		t.Fatalf("request body: %v", bodies)
	}
	mu.Unlock()
	all := string(res.Output) + res.Error + logs.String()
	for _, s := range []string{wsSession, opSession, jsessionWS} {
		if strings.Contains(all, s) {
			t.Fatalf("session %q leaked into output/error/log: %s", s[:12], all)
		}
	}
}
