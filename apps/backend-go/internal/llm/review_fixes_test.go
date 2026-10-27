package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// sourced is a Settings that also reports the level each value came from (WorkspaceSettings does).
type sourced struct{ ws, global map[string]string }

func (s sourced) Get(ctx context.Context, key, def string) (string, error) {
	v, _, err := s.GetSource(ctx, key, def)
	return v, err
}

func (s sourced) GetSource(_ context.Context, key, def string) (string, string, error) {
	if v := s.ws[key]; v != "" {
		return v, "workspace", nil
	}
	if v := s.global[key]; v != "" {
		return v, "global", nil
	}
	return def, "default", nil
}

// M5: the base URL comes from the same level as the key.
func TestProviderBaseURLFollowsTheKeyLevel(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		s       sourced
		wantURL string
	}{
		{"global key never pairs with a workspace URL",
			sourced{ws: map[string]string{"GROQ_BASE_URL": "http://evil.example/v1"}, global: map[string]string{"GROQ_API_KEY": "gk"}},
			"https://api.groq.com/openai/v1"},
		{"global key with global URL",
			sourced{ws: map[string]string{}, global: map[string]string{"GROQ_API_KEY": "gk", "GROQ_BASE_URL": "http://global.example/v1"}},
			"http://global.example/v1"},
		{"workspace key with workspace URL",
			sourced{ws: map[string]string{"GROQ_API_KEY": "wk", "GROQ_BASE_URL": "http://ws.example/v1"}, global: map[string]string{"GROQ_BASE_URL": "http://global.example/v1"}},
			"http://ws.example/v1"},
		{"workspace key does not take the global URL either",
			sourced{ws: map[string]string{"GROQ_API_KEY": "wk"}, global: map[string]string{"GROQ_BASE_URL": "http://global.example/v1"}},
			"https://api.groq.com/openai/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, nil, nil, func(o *Options) { o.Settings = tc.s })
			p, ok := c.providerConfig(ctx, "groq")
			if !ok || p.BaseURL != tc.wantURL {
				t.Fatalf("ok=%v base=%q, want %q", ok, p.BaseURL, tc.wantURL)
			}
		})
	}
}

// Rate-limit storms: total attempts per Complete are capped (MaxAttemptsPerComplete) and every
// configured provider (up to three) is still tried once.
func TestCompleteAttemptsAreCapped(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{
		"GROQ_API_KEY": "k", "GROQ_BASE_URL": srv.URL, "CEREBRAS_API_KEY": "k", "CEREBRAS_BASE_URL": srv.URL,
		"MISTRAL_API_KEY": "k", "MISTRAL_BASE_URL": srv.URL, "LLM_DEFAULT_PROVIDER": "groq",
	}, nil, nil)
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrAllFailed) {
		t.Fatalf("err = %v", err)
	}
	if n := int(hits.Load()); n > MaxAttemptsPerComplete || n < 3 {
		t.Fatalf("%d requests, want 3..%d (every provider tried, total capped)", n, MaxAttemptsPerComplete)
	}
}

// The Anthropic SDK policy already retried (3 requests); the outer rate-limit loop must not multiply it.
func TestNativeProviderIsNotRetriedTwice(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": srv.URL, "LLM_DEFAULT_PROVIDER": "anthropic"}, nil, nil)
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err == nil {
		t.Fatal("want error")
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("%d requests, want 3 (SDK policy only)", n)
	}
}

// A call that failed at the transport/provider level is not asked again by ExtractJSON.
func TestExtractJSONDoesNotRetryAFailedComplete(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{"GROQ_API_KEY": "k", "GROQ_BASE_URL": srv.URL, "LLM_DEFAULT_PROVIDER": "groq"}, nil, nil)
	if _, err := c.ExtractJSON(context.Background(), "p", "", 0); err == nil {
		t.Fatal("want error")
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// Transport errors name the host, never the URL (path, query, userinfo).
func TestTransportErrorsCarryHostOnly(t *testing.T) {
	const base = "http://user:pw@127.0.0.1:1/v1/secret-path?key=abc123"
	for _, id := range []string{"groq", "anthropic"} {
		c := newTestClient(t, mapSettings{
			strings.ToUpper(id) + "_API_KEY": "k", strings.ToUpper(id) + "_BASE_URL": base, "LLM_DEFAULT_PROVIDER": id,
		}, nil, nil)
		_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
		if err == nil {
			t.Fatal("want error")
		}
		all := err.Error() + " " + strings.Join(c.Usage().Errors, " ")
		for _, leak := range []string{"secret-path", "abc123", "user:pw", "pw@"} {
			if strings.Contains(all, leak) {
				t.Fatalf("%s: error leaks %q: %s", id, leak, all)
			}
		}
		if !strings.Contains(all, "127.0.0.1:1") {
			t.Fatalf("%s: error should name the host: %s", id, all)
		}
	}
}

// A hostile provider cannot stream an endless line.
func TestStreamLineOverflowIsAnError(t *testing.T) {
	old := maxStreamLine
	maxStreamLine = 1 << 10
	defer func() { maxStreamLine = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + strings.Repeat("x", 64<<10)))
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{}, nil, nil)
	var sawErr bool
	for f := range c.Stream(context.Background(), StreamRequest{Cursor: Cursor{Provider: ChatProvider{ID: "p", APIKey: "k", BaseURL: srv.URL}}}) {
		if f.Kind == FrameError && strings.Contains(f.Text, "too long") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("an over-long stream line must end with an error frame")
	}
}

func TestBatchResultLineOverflowIsAnError(t *testing.T) {
	old := maxBatchLine
	maxBatchLine = 1 << 10
	defer func() { maxBatchLine = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("y", 64<<10)))
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{}, nil, nil)
	p := Provider{ID: "anthropic", APIKey: "k", BaseURL: srv.URL, Native: true}
	err := c.readBatchResults(context.Background(), p, "/v1/messages/batches/b1/results", map[string]bool{}, map[string]string{}, BatchOptions{})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("err = %v, want a line-too-long error", err)
	}
}
