package credentials

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const tok = "svc-token-SECRET"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) Add(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// server returns values keyed by "<ws>/<var>" and counts hits.
func server(t *testing.T, values map[string]string, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(401)
			return
		}
		v := values[r.Header.Get("X-Workspace-Id")+"/"+strings.TrimPrefix(r.URL.Path, "/internal/v1/credentials/")]
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(`{"value":"` + v + `"}`))
	}))
}

func newTest(t *testing.T, srv *httptest.Server) (*Client, *fakeClock) {
	c := New(srv.URL, tok, nil)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c.now = clk.Now
	return c, clk
}

func TestSendsBearerAndWorkspaceHeader(t *testing.T) {
	var gotAuth, gotWS, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotWS, gotPath = r.Header.Get("Authorization"), r.Header.Get("X-Workspace-Id"), r.URL.Path
		w.Write([]byte(`{"value":"sk-1"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, tok, nil)
	v, err := c.Get(context.Background(), "ws1", "LEADMAGIC_API_KEY")
	if err != nil || v != "sk-1" {
		t.Fatalf("got %q, %v", v, err)
	}
	if gotAuth != "Bearer "+tok || gotWS != "ws1" || gotPath != "/internal/v1/credentials/LEADMAGIC_API_KEY" {
		t.Fatalf("bad request: %q %q %q", gotAuth, gotWS, gotPath)
	}
}

func TestCacheIsPerWorkspace(t *testing.T) {
	var hits int32
	srv := server(t, map[string]string{"a/K": "val-a", "b/K": "val-b"}, &hits)
	defer srv.Close()
	c, _ := newTest(t, srv)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if v, _ := c.Get(ctx, "a", "K"); v != "val-a" {
			t.Fatalf("a: %q", v)
		}
		if v, _ := c.Get(ctx, "b", "K"); v != "val-b" {
			t.Fatalf("b: %q", v)
		}
	}
	if hits != 2 {
		t.Fatalf("want 2 fetches (one per workspace), got %d", hits)
	}
}

func TestCacheExpires(t *testing.T) {
	var hits int32
	vals := map[string]string{"a/K": "v1"}
	srv := server(t, vals, &hits)
	defer srv.Close()
	c, clk := newTest(t, srv)
	ctx := context.Background()
	c.Get(ctx, "a", "K")
	clk.Add(TTL - time.Second)
	c.Get(ctx, "a", "K")
	if hits != 1 {
		t.Fatalf("within TTL should hit cache, hits=%d", hits)
	}
	clk.Add(2 * time.Second)
	vals["a/K"] = "v2" // safe: server handler reads under no concurrency here
	if v, _ := c.Get(ctx, "a", "K"); v != "v2" || hits != 2 {
		t.Fatalf("after TTL want refetch v2, got %q hits=%d", v, hits)
	}
}

func TestEmptyValueNotCached(t *testing.T) {
	var hits int32
	vals := map[string]string{}
	srv := server(t, vals, &hits)
	defer srv.Close()
	c, _ := newTest(t, srv)
	ctx := context.Background()
	if v, err := c.Get(ctx, "a", "K"); v != "" || err != nil {
		t.Fatalf("empty must be (\"\", nil), got %q %v", v, err)
	}
	vals["a/K"] = "added-later"
	if v, _ := c.Get(ctx, "a", "K"); v != "added-later" {
		t.Fatalf("key added later must take effect immediately, got %q", v)
	}
}

func TestErrorsNotCachedAndLeakNothing(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			// A hostile/buggy upstream echoing secrets must not reach errors.
			w.WriteHeader(500)
			w.Write([]byte("boom " + tok + " sk-leaked-value"))
			return
		}
		w.Write([]byte(`{"value":"sk-ok"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, tok, nil)
	_, err := c.Get(context.Background(), "ws", "K")
	if err == nil {
		t.Fatal("5xx must be an error")
	}
	for _, bad := range []string{tok, "sk-leaked-value"} {
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("error leaks %q: %v", bad, err)
		}
	}
	fail.Store(false)
	if v, err := c.Get(context.Background(), "ws", "K"); err != nil || v != "sk-ok" {
		t.Fatalf("error was cached: %q %v", v, err)
	}
}

func TestStatusErrors(t *testing.T) {
	for _, code := range []int{401, 404, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		c := New(srv.URL, tok, nil)
		if v, err := c.Get(context.Background(), "ws", "K"); err == nil || v != "" {
			t.Fatalf("status %d: want error, got %q %v", code, v, err)
		}
		srv.Close()
	}
}

func TestTransportAndMalformedErrorsLeakNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"value": sk-secret-garbage`))
	}))
	c := New(srv.URL, tok, nil)
	_, err := c.Get(context.Background(), "ws", "K")
	if err == nil || strings.Contains(err.Error(), "sk-secret-garbage") || strings.Contains(err.Error(), tok) {
		t.Fatalf("malformed: %v", err)
	}
	srv.Close()
	_, err = c.Get(context.Background(), "ws", "K2")
	if err == nil || strings.Contains(err.Error(), tok) {
		t.Fatalf("transport: %v", err)
	}
}

func TestRequiresIDs(t *testing.T) {
	c := New("http://127.0.0.1:1", tok, nil)
	if _, err := c.Get(context.Background(), "", "K"); err == nil {
		t.Fatal("empty workspace must error")
	}
	if _, err := c.Get(context.Background(), "w", ""); err == nil {
		t.Fatal("empty env var must error")
	}
}

func TestDefaultClientTimeout(t *testing.T) {
	if New("http://x", tok, nil).hc.Timeout != 5*time.Second {
		t.Fatal("default client must have a 5s timeout")
	}
}

func TestManifestFetchCachesAndSendsBearer(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/internal/v1/manifests/vend" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"manifest":{"name":"vend"},"sha256":"abc"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, tok, nil)
	raw, sha, err := c.Manifest(context.Background(), "vend")
	if err != nil || sha != "abc" || string(raw) != `{"name":"vend"}` {
		t.Fatalf("%s %s %v", raw, sha, err)
	}
	if _, _, err := c.Manifest(context.Background(), "vend"); err != nil || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("not cached: hits=%d err=%v", hits, err)
	}
	if _, _, err := c.Manifest(context.Background(), "nope"); !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDoesNotFollowRedirects(t *testing.T) {
	var evil int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&evil, 1)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()
	c := New(srv.URL, tok, nil)
	if _, err := c.Get(context.Background(), "ws", "VEND_KEY"); err == nil {
		t.Fatal("redirect response must be an error")
	}
	if _, _, err := c.Manifest(context.Background(), "vend"); err == nil {
		t.Fatal("redirect response must be an error")
	}
	if atomic.LoadInt32(&evil) != 0 {
		t.Fatal("redirect was followed with the bearer token")
	}
}
