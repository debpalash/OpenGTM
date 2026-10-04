package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLegacy imitates /api/auth/workspace-context: token "good" belongs to
// ws-1 (editor) and ws-2 (owner, also the active workspace).
type fakeLegacy struct {
	calls  atomic.Int64
	status atomic.Int64 // forced status when non-zero
	mu     sync.Mutex
	seen   []http.Header
}

func (f *fakeLegacy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.mu.Lock()
	f.seen = append(f.seen, r.Header.Clone())
	f.mu.Unlock()
	if r.URL.Path != ContextPath {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s := f.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		json.NewEncoder(w).Encode(map[string]string{"detail": "forced"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer good" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"detail": "Could not validate credentials"})
		return
	}
	role := map[string]string{"ws-1": "editor", "ws-2": "owner", "": "owner"}
	ws := r.Header.Get("X-Workspace-Id")
	r2, ok := role[ws]
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"detail": "Workspace access denied"})
		return
	}
	if ws == "" {
		ws = "ws-2"
	}
	json.NewEncoder(w).Encode(Workspace{UserID: "7", Username: "ana", WorkspaceID: ws, Slug: ws + "-slug", Role: r2})
}

func newClient(t *testing.T, opts Options) (*Client, *fakeLegacy) {
	t.Helper()
	fake := &fakeLegacy{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/", opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, fake
}

func TestResolveForwardsAndCaches(t *testing.T) {
	c, fake := newClient(t, Options{})
	ctx := context.Background()
	ws, err := c.Resolve(ctx, "good", "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	want := Workspace{UserID: "7", Username: "ana", WorkspaceID: "ws-1", Slug: "ws-1-slug", Role: "editor"}
	if ws != want {
		t.Fatalf("Resolve = %+v", ws)
	}
	if h := fake.seen[0]; h.Get("Authorization") != "Bearer good" || h.Get("X-Workspace-Id") != "ws-1" {
		t.Fatalf("forwarded headers %v", h)
	}
	for range 5 {
		if _, err := c.Resolve(ctx, "good", "ws-1"); err != nil {
			t.Fatal(err)
		}
	}
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("expected 1 upstream call with cache, got %d", n)
	}
	// The cache key includes the workspace.
	ws, err = c.Resolve(ctx, "good", "ws-2")
	if err != nil || ws.Role != "owner" || fake.calls.Load() != 2 {
		t.Fatalf("workspace not part of the cache key: %+v %v calls=%d", ws, err, fake.calls.Load())
	}
	// Empty workspace means the active one and is forwarded without the header.
	ws, err = c.Resolve(ctx, "good", "")
	if err != nil || ws.WorkspaceID != "ws-2" || fake.seen[2].Get("X-Workspace-Id") != "" {
		t.Fatalf("active workspace: %+v %v", ws, err)
	}
}

func TestFailuresPassThroughAndAreNeverCached(t *testing.T) {
	c, fake := newClient(t, Options{})
	ctx := context.Background()
	cases := []struct {
		token, ws string
		status    int
		detail    string
	}{
		{"", "ws-1", 401, "Not authenticated"},
		{"bad", "ws-1", 401, "Could not validate credentials"},
		{"good", "ws-9", 403, "Workspace access denied"},
	}
	for _, tc := range cases {
		for range 2 {
			_, err := c.Resolve(ctx, tc.token, tc.ws)
			var se *StatusError
			if !errors.As(err, &se) || se.Status != tc.status || se.Detail != tc.detail {
				t.Fatalf("Resolve(%q,%q) err = %v", tc.token, tc.ws, err)
			}
		}
	}
	// Empty token short-circuits; the other two cases hit upstream twice each.
	if n := fake.calls.Load(); n != 4 {
		t.Fatalf("failures must not be cached: %d upstream calls, want 4", n)
	}

	fake.status.Store(http.StatusInternalServerError)
	if _, err := c.Resolve(ctx, "good", "ws-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("5xx err = %v, want ErrUnavailable", err)
	}
	fake.status.Store(0)
	if _, err := c.Resolve(ctx, "good", "ws-1"); err != nil {
		t.Fatalf("recovery after outage: %v", err)
	}
}

func TestUnreachableIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, _ := New(url, Options{})
	if _, err := c.Resolve(context.Background(), "good", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestCacheExpiresAndIsBounded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, fake := newClient(t, Options{MaxEntries: 2, Now: func() time.Time { return now }})
	ctx := context.Background()
	c.Resolve(ctx, "good", "ws-1")
	now = now.Add(29 * time.Second)
	c.Resolve(ctx, "good", "ws-1")
	if fake.calls.Load() != 1 {
		t.Fatal("entry expired before 30s")
	}
	now = now.Add(2 * time.Second)
	c.Resolve(ctx, "good", "ws-1")
	if fake.calls.Load() != 2 {
		t.Fatal("entry served after 30s")
	}
	c.Resolve(ctx, "good", "ws-2")
	c.Resolve(ctx, "good", "")
	c.mu.Lock()
	n := len(c.cache)
	c.mu.Unlock()
	if n > 2 {
		t.Fatalf("cache holds %d entries, bound is 2", n)
	}
}

func TestCredentialsFromHeaderOrQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v2/events?token=q&workspace_id=wq", nil)
	if tok, ws := Credentials(r); tok != "q" || ws != "wq" {
		t.Fatalf("query credentials = %q %q", tok, ws)
	}
	r.Header.Set("Authorization", "Bearer h")
	r.Header.Set("X-Workspace-Id", "wh")
	if tok, ws := Credentials(r); tok != "h" || ws != "wh" {
		t.Fatalf("header credentials = %q %q", tok, ws)
	}
	r.Header.Set("Authorization", "Basic abc")
	if tok, _ := Credentials(r); tok != "q" {
		t.Fatalf("non-bearer scheme used as token: %q", tok)
	}
}

func TestMiddlewareAndRequireRole(t *testing.T) {
	c, _ := newClient(t, Options{})
	var got Workspace
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
		if err := RequireRole(r.Context(), "admin"); err != nil {
			WriteError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	do := func(auth, ws string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/x", nil)
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		r.Header.Set("X-Workspace-Id", ws)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("", "ws-1"); w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("anonymous: %d %v", w.Code, w.Header())
	}
	if w := do("good", "ws-9"); w.Code != 403 {
		t.Fatalf("non-member: %d", w.Code)
	}
	if w := do("good", "ws-1"); w.Code != 403 || got.Role != "editor" {
		t.Fatalf("editor requiring admin: %d %+v", w.Code, got)
	}
	if w := do("good", "ws-2"); w.Code != 204 || got.Role != "owner" {
		t.Fatalf("owner must satisfy any role: %d %+v", w.Code, got)
	}

	ctx := WithWorkspace(context.Background(), Workspace{Role: "viewer"})
	if RequireRole(ctx, "viewer", "editor") != nil || RequireRole(ctx, "editor") == nil {
		t.Fatal("RequireRole role matching")
	}
	if RequireRole(WithWorkspace(context.Background(), Workspace{}), "") == nil {
		t.Fatal("empty role must not satisfy an empty requirement")
	}
	if RequireRole(context.Background(), "viewer") == nil {
		t.Fatal("missing authorization must fail")
	}
}
