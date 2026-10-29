package settings

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

func newProviders(t *testing.T, st *fakeStore, allowHosts ...string) *Providers {
	t.Helper()
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	secret := map[string]bool{}
	for _, k := range reg.SecretAppKeys {
		secret[k] = true
	}
	app, _ := testApp(t, st, nil)
	app.Secret = func(k string) bool { return secret[k] }
	return &Providers{Reg: reg, App: app, HTTP: egress.NewClientForEnv("test", allowHosts), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestProvidersMaskShortAndLongKeys(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abc", "••••••••"},
		{"123456789012345", "••••••••"},
		{"sk-abcdefgh1234567890", "sk-abcde••••7890"},
		{"12345678901234567890", "12345678••••7890"},
	}
	for _, c := range cases {
		if got := MaskKey(c.in); got != c.want {
			t.Errorf("MaskKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUndecryptableKeyDoesNotLeakOrFailList(t *testing.T) {
	st := newFake()
	st.m["OPENROUTER_API_KEY"] = "enc:v1:gAAAAAB-garbage-token-xyz"
	p := newProviders(t, st)
	views, def, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if def != "openrouter" {
		t.Fatalf("default %q", def)
	}
	raw, _ := json.Marshal(views)
	if strings.Contains(string(raw), "garbage-token") {
		t.Fatal("ciphertext leaked into list")
	}
	var found bool
	for _, v := range views {
		if v.ID == "openrouter" {
			found = true
			if !v.Configured || v.APIKeyMasked != "••••••••" {
				t.Fatalf("openrouter view: %+v", v)
			}
		}
	}
	if !found {
		t.Fatal("openrouter missing")
	}
}

func TestTestProviderUnconfigured200(t *testing.T) {
	p := newProviders(t, newFake())
	res, ok, err := p.Test(context.Background(), "openrouter")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if res["status"] != "error" || res["error"] != "API key not configured" || len(res) != 2 {
		t.Fatalf("%v", res)
	}
	if _, ok, _ := p.Test(context.Background(), "nope"); ok {
		t.Fatal("unknown provider found")
	}
}

func TestTestProviderBlocksPrivateBaseURL(t *testing.T) {
	st := newFake()
	p := newProviders(t, st) // no allowlist: loopback blocked
	ctx := context.Background()
	if err := p.App.Set(ctx, "OPENROUTER_API_KEY", "sk-secret-value-123456"); err != nil {
		t.Fatal(err)
	}
	if err := p.App.Set(ctx, "OPENROUTER_BASE_URL", "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	res, _, err := p.Test(ctx, "openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "error" {
		t.Fatalf("%v", res)
	}
	msg, _ := res["error"].(string)
	if !strings.Contains(msg, "blocked") || strings.Contains(msg, "sk-secret") {
		t.Fatalf("error %q", msg)
	}
}

func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	h, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestTestProviderAnthropicShape(t *testing.T) {
	var gotPath, gotKey, gotVer string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotVer = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"model":"claude-x","content":[{"type":"text","text":" hel"},{"type":"tool_use"},{"type":"text","text":"lo "}]}`))
	}))
	defer srv.Close()
	st := newFake()
	p := newProviders(t, st, hostOf(t, srv))
	ctx := context.Background()
	_ = p.App.Set(ctx, "ANTHROPIC_API_KEY", "sk-ant-1234567890abcdef")
	_ = p.App.Set(ctx, "ANTHROPIC_BASE_URL", srv.URL+"/")
	_ = p.App.Set(ctx, "ANTHROPIC_MODEL", "claude-test")
	res, _, err := p.Test(ctx, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" || gotKey != "sk-ant-1234567890abcdef" || gotVer != "2023-06-01" {
		t.Fatalf("request: %s %s %s", gotPath, gotKey, gotVer)
	}
	if gotBody["model"] != "claude-test" || gotBody["max_tokens"] != float64(50) {
		t.Fatalf("body %v", gotBody)
	}
	if res["status"] != "ok" || res["response"] != "hello" || res["model"] != "claude-x" {
		t.Fatalf("%v", res)
	}
}

func TestTestProviderOpenAIShapeAndCerebras(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":null,"reasoning":" thinking "}}]}`))
	}))
	defer srv.Close()
	p := newProviders(t, newFake(), hostOf(t, srv))
	ctx := context.Background()
	_ = p.App.Set(ctx, "CEREBRAS_API_KEY", "csk-1234567890abcdef")
	_ = p.App.Set(ctx, "CEREBRAS_BASE_URL", srv.URL)
	res, _, err := p.Test(ctx, "cerebras")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/chat/completions" || gotAuth != "Bearer csk-1234567890abcdef" {
		t.Fatalf("%s %s", gotPath, gotAuth)
	}
	if _, has := gotBody["max_tokens"]; has || gotBody["max_completion_tokens"] != float64(50) {
		t.Fatalf("body %v", gotBody)
	}
	if res["status"] != "ok" || res["response"] != "thinking" || res["model"] == nil {
		t.Fatalf("%v", res)
	}
}

func TestTestProviderErrorBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()
	p := newProviders(t, newFake(), hostOf(t, srv))
	ctx := context.Background()
	_ = p.App.Set(ctx, "OPENROUTER_API_KEY", "sk-1234567890abcdefgh")
	_ = p.App.Set(ctx, "OPENROUTER_BASE_URL", srv.URL)
	res, _, _ := p.Test(ctx, "openrouter")
	if res["status"] != "error" || res["error"] != "bad key" {
		t.Fatalf("%v", res)
	}
}

func TestTestProviderTransportErrorLeaksNeitherKeyNorURL(t *testing.T) {
	const key = "AIzaSyTESTKEY1234567890abcdef"
	var buf strings.Builder
	st := newFake()
	p := newProviders(t, st)
	p.Log = slog.New(slog.NewTextHandler(&buf, nil))
	// google_ai id is required for the native branch: reuse it as non-openai.
	prov, _ := p.Reg.Provider("google_ai")
	prov.OpenAICompatible = false
	p.Reg.Providers.m["google_ai"] = prov
	ctx := context.Background()
	_ = p.App.Set(ctx, prov.EnvKey, key)
	_ = p.App.Set(ctx, prov.EnvURL, "http://127.0.0.1:1/v1beta")
	res, _, err := p.Test(ctx, "google_ai")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(res)
	all := string(out) + buf.String()
	for _, bad := range []string{key, "generateContent", "127.0.0.1:1/v1beta"} {
		if strings.Contains(all, bad) {
			t.Fatalf("leak of %q in %s", bad, all)
		}
	}
	if res["status"] != "error" {
		t.Fatalf("%v", res)
	}
}

func TestGoogleKeySentAsHeaderNotQuery(t *testing.T) {
	var q, hk, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, hk, path = r.URL.RawQuery, r.Header.Get("x-goog-api-key"), r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":" hi "}]}}]}`))
	}))
	defer srv.Close()
	p := newProviders(t, newFake(), hostOf(t, srv))
	prov, _ := p.Reg.Provider("google_ai")
	prov.OpenAICompatible = false
	p.Reg.Providers.m["google_ai"] = prov
	ctx := context.Background()
	_ = p.App.Set(ctx, prov.EnvKey, "AIzaSy1234567890abcdef")
	_ = p.App.Set(ctx, prov.EnvURL, srv.URL+"/v1beta/")
	_ = p.App.Set(ctx, prov.EnvModel, "gem/ini 1")
	res, _, _ := p.Test(ctx, "google_ai")
	if q != "" || hk != "AIzaSy1234567890abcdef" || path != "/v1beta/models/gem%2Fini%201:generateContent" || res["response"] != "hi" {
		t.Fatalf("q=%q hk=%q path=%q res=%v", q, hk, path, res)
	}
}
