package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

func okBody() string {
	return `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
}

func newTestClient(t *testing.T, s mapSettings, hc *http.Client, mod func(*Options)) *Client {
	t.Helper()
	o := Options{Settings: s, HTTP: hc, Sleep: (&sleepLog{}).Sleep, Jitter: func() float64 { return 0 }}
	if hc == nil {
		o.HTTP = http.DefaultClient
	}
	if mod != nil {
		mod(&o)
	}
	return New(o)
}

// Python matched the bare substring "rate", so any error mentioning "generate"
// or "moderate" was retried with backoff. Go needs the phrase.
func TestRateLimitDetectionIsNotASubstringOfOtherWords(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"HTTP 429: x", true}, {"HTTP 400: Rate limit reached", true}, {"HTTP 403: exceeded your current quota", true},
		{"HTTP 400: too many requests", true}, {"rate_limit_error", true},
		{"HTTP 400: cannot generate this", false}, {"HTTP 400: moderate content", false}, {"HTTP 500: boom", false},
	} {
		if got := isRateLimited(errors.New(tc.msg)); got != tc.want {
			t.Errorf("%q: want %v got %v", tc.msg, tc.want, got)
		}
	}
}

func TestNonJSONErrorPageReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html>Bad gateway</html>"))
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{"GROQ_API_KEY": "k", "GROQ_BASE_URL": srv.URL, "LLM_DEFAULT_PROVIDER": "groq"}, nil, nil)
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrAllFailed) || !strings.Contains(err.Error(), "HTTP 502: <html>Bad gateway</html>") {
		t.Fatalf("got %v", err)
	}
}

func TestCompleteSentinelErrors(t *testing.T) {
	c := newTestClient(t, mapSettings{}, nil, nil)
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); !errors.Is(err, ErrNoProvider) || err.Error() != "No LLM providers configured" {
		t.Fatalf("no provider: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"  "}}],"usage":{}}`))
	}))
	defer srv.Close()
	c = newTestClient(t, mapSettings{"GROQ_API_KEY": "k", "GROQ_BASE_URL": srv.URL}, nil, nil)
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("empty: %v", err)
	}
}

func TestCompleteStopsOnCancelledContext(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":{"message":"rate limit"}}`, 429)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(t, mapSettings{"GROQ_API_KEY": "k", "GROQ_BASE_URL": srv.URL}, nil, func(o *Options) {
		o.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	})
	if _, err := c.Complete(ctx, CompleteRequest{Prompt: "p"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("requests after cancel: %d", hits.Load())
	}
}

func TestCompleteTimeoutMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{"GROQ_API_KEY": "k", "GROQ_BASE_URL": srv.URL}, nil, func(o *Options) {
		o.Config = config.LLMSettings{HTTPTimeout: 100 * time.Millisecond}
	})
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrAllFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if got := c.Usage().Errors; len(got) != 1 || !strings.HasPrefix(got[0], "groq: request timed out") {
		t.Fatalf("usage errors %q", got)
	}
}

func TestOpenAIRateHeadersGarbageDoNotDropUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "unlimited") // Python: int() raised and the row was lost
		_, _ = w.Write([]byte(okBody()))
	}))
	defer srv.Close()
	sink := &usageSink{}
	c := newTestClient(t, mapSettings{"GROQ_API_KEY": "k", "GROQ_BASE_URL": srv.URL}, nil, func(o *Options) {
		o.Usage, o.WorkspaceID = sink, "ws-test"
	})
	if got, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err != nil || got != "ok" {
		t.Fatalf("%q %v", got, err)
	}
	if len(sink.rows) != 1 || sink.rows[0].RateLimit != 0 {
		t.Fatalf("rows %+v", sink.rows)
	}
}

func TestProviderCacheTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	s := mapSettings{"GROQ_API_KEY": "k"}
	c := newTestClient(t, s, nil, func(o *Options) { o.Now = func() time.Time { return now } })
	ctx := context.Background()
	if n := len(c.Providers(ctx)); n != 1 {
		t.Fatalf("providers %d", n)
	}
	s["CEREBRAS_API_KEY"] = "c"
	now = now.Add(59 * time.Second)
	if n := len(c.Providers(ctx)); n != 1 {
		t.Fatalf("cache must hold for 60s, got %d", n)
	}
	now = now.Add(2 * time.Second)
	if n := len(c.Providers(ctx)); n != 2 {
		t.Fatalf("cache must expire after 60s, got %d", n)
	}
}

func TestAnthropicErrorsAreRetriedLikeTheSDK(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("x-should-retry", "false")
		http.Error(w, `{"type":"error","error":{"message":"no retry wanted"}}`, 529)
	}))
	defer srv.Close()
	sl := &sleepLog{}
	c := newTestClient(t, mapSettings{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": srv.URL, "LLM_DEFAULT_PROVIDER": "anthropic"}, nil, func(o *Options) { o.Sleep = sl.Sleep })
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err == nil {
		t.Fatal("want error")
	}
	if hits.Load() != 1 {
		t.Fatalf("x-should-retry:false must stop retries, got %d requests", hits.Load())
	}

	// Backoff without Retry-After: 0.5s then 1s (jitter 0), two retries.
	hits.Store(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.Error(w, `{}`, 500) }))
	defer srv2.Close()
	sl = &sleepLog{}
	c = newTestClient(t, mapSettings{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": srv2.URL, "LLM_DEFAULT_PROVIDER": "anthropic"}, nil, func(o *Options) { o.Sleep = sl.Sleep })
	_, _ = c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if hits.Load() != 3 {
		t.Fatalf("want 1+2 requests, got %d", hits.Load())
	}
	// SDK retries 0.5s,1s; then the outer failover loop saw no rate-limit text.
	if len(sl.d) != 2 || sl.d[0] != 0.5 || sl.d[1] != 1 {
		t.Fatalf("backoff %v", sl.d)
	}
}

func TestAnthropicTimeoutAndConnectionErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": srv.URL, "LLM_DEFAULT_PROVIDER": "anthropic"}, nil, func(o *Options) {
		o.Config = config.LLMSettings{HTTPTimeout: 50 * time.Millisecond}
	})
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "Request timed out.") {
		t.Fatalf("got %v", err)
	}
	c = newTestClient(t, mapSettings{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "http://127.0.0.1:1", "LLM_DEFAULT_PROVIDER": "anthropic"}, nil, nil)
	if _, err = c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "Connection error") {
		t.Fatalf("got %v", err)
	}
}

func TestAnthropicDefaultHostForCanonicalBaseURLs(t *testing.T) {
	for _, b := range []string{"", "https://api.anthropic.com", "https://api.anthropic.com/v1"} {
		if got := anthropicBase(Provider{BaseURL: b}); got != "https://api.anthropic.com" {
			t.Errorf("%q -> %q", b, got)
		}
	}
	if got := anthropicBase(Provider{BaseURL: "http://proxy:8080/x/"}); got != "http://proxy:8080/x" {
		t.Errorf("override: %q", got)
	}
}

func TestBatchShouldStopCancelsAndReturnsNothing(t *testing.T) {
	var cancelled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/messages/batches":
			_, _ = w.Write([]byte(`{"id":"b1","processing_status":"in_progress"}`))
		case r.Method == "POST" && r.URL.Path == "/v1/messages/batches/b1/cancel":
			cancelled.Store(true)
			_, _ = w.Write([]byte(`{"id":"b1","processing_status":"canceling"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": srv.URL, "LLM_DEFAULT_PROVIDER": "anthropic"}, nil, nil)
	got, err := c.BatchComplete(context.Background(), []BatchRequest{{CustomID: "a", Prompt: "p"}}, BatchOptions{ShouldStop: func() bool { return true }})
	if err != nil || len(got) != 0 || !cancelled.Load() {
		t.Fatalf("got %v err=%v cancelled=%v", got, err, cancelled.Load())
	}
}

func TestBatchStrictModeSurfacesStillProcessing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"b1","processing_status":"in_progress"}`))
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": srv.URL, "LLM_DEFAULT_PROVIDER": "anthropic"}, nil, nil)
	_, err := c.BatchComplete(context.Background(), []BatchRequest{{CustomID: "a", Prompt: "p"}},
		BatchOptions{PollInterval: time.Second, MaxWait: 2 * time.Second, StrictResults: true})
	if err == nil || !strings.Contains(err.Error(), "still processing") {
		t.Fatalf("got %v", err)
	}
}

func TestStreamCancelEndsWithoutFurtherFrames(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch := c.Stream(ctx, StreamRequest{Messages: []any{}, Tools: []any{}, Cursor: Cursor{Provider: ChatProvider{ID: "p", APIKey: "k", BaseURL: srv.URL}}})
	f := <-ch
	if f.Kind != FrameContent || f.Text != "a" {
		t.Fatalf("first frame %+v", f)
	}
	<-started
	cancel()
	for f := range ch { // must close without an error frame
		if f.Kind == FrameError {
			t.Fatalf("error frame after cancel: %+v", f)
		}
	}
}

func TestStreamMidStreamResetIsAProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000") // promise more than we send
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n"))
	}))
	defer srv.Close()
	c := newTestClient(t, mapSettings{}, nil, nil)
	var kinds []string
	for f := range c.Stream(context.Background(), StreamRequest{Cursor: Cursor{Provider: ChatProvider{ID: "p", APIKey: "k", BaseURL: srv.URL}}}) {
		kinds = append(kinds, f.Kind)
		if f.Kind == FrameError && !strings.HasPrefix(f.Text, "AI provider error: ") {
			t.Errorf("error text %q", f.Text)
		}
	}
	if strings.Join(kinds, ",") != "content,error,cursor" {
		t.Fatalf("frames %v", kinds)
	}
}

func TestClientDoesNotLeakKeysInStringForms(t *testing.T) {
	p := Provider{ID: "groq", APIKey: "sk-secret"}
	cp := ChatProvider{ID: "groq", APIKey: "sk-secret"}
	for _, s := range []string{p.String(), cp.String()} {
		if strings.Contains(s, "sk-secret") {
			t.Fatal("key leaked: " + s)
		}
	}
}

func TestParseObjectEdgeCases(t *testing.T) {
	for in, want := range map[string]string{
		"see {foo} then {\"a\": 1}":         `a`,
		"```json\n{\"a\": {\"b\": 1}}\n```": `a`,
		"\uFEFF{\"a\": 1}":                  `a`,
	} {
		o, ok := ParseObject(in)
		if _, has := o[want]; !ok || !has {
			t.Errorf("%q -> %v %v", in, o, ok)
		}
	}
	for _, in := range []string{"", "{", "}{", `{"a":`, "[]", "null", "{}x{"} {
		if o, ok := ParseObject(in); ok && len(o) > 0 {
			t.Errorf("%q -> %v", in, o)
		}
	}
}
