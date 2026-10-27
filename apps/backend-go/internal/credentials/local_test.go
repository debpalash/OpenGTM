package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// memStore is an in-memory secrets.Store holding raw (possibly encrypted) values.
type memStore struct {
	mu    sync.Mutex
	app   map[string]string
	ws    map[string]string // "<ws>/<key>"
	err   error
	reads int
}

func (s *memStore) AppSetting(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.app[key], s.err
}

func (s *memStore) WorkspaceSetting(_ context.Context, ws, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	v, ok := s.ws[ws+"/"+key]
	return v, ok, s.err
}

func (s *memStore) set(ws, key, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ws == "" {
		s.app[key] = v
	} else {
		s.ws[ws+"/"+key] = v
	}
}

func newMem() *memStore { return &memStore{app: map[string]string{}, ws: map[string]string{}} }

func testEnvelope(t *testing.T) *secrets.Envelope {
	t.Helper()
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: "local-creds-test", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func allowOnly(vars ...string) func(string) bool {
	return func(v string) bool {
		for _, x := range vars {
			if x == v {
				return true
			}
		}
		return false
	}
}

func newLocalTest(t *testing.T, st *memStore, env map[string]string, allow func(string) bool) (*Local, *fakeClock) {
	t.Helper()
	r := secrets.NewResolver(testEnvelope(t), st, func(k string) string { return env[k] })
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return NewLocal(r, allow, clk.Now), clk
}

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

// TestLocalSemantics is the table the former HTTP client shared with Local (the HTTP source is gone).
func TestLocalSemantics(t *testing.T) {
	type impl struct {
		name string
		get  func(ws, env string) (string, error)
		set  func(ws, env, v string)
		adv  func(d time.Duration)
	}
	mk := map[string]func(t *testing.T) impl{
		"local": func(t *testing.T) impl {
			st := newMem()
			l, clk := newLocalTest(t, st, nil, allowOnly("OK_KEY"))
			return impl{"local",
				func(ws, env string) (string, error) { return l.Get(context.Background(), ws, env) },
				func(ws, env, v string) { st.set(ws, env, v) },
				clk.Add}
		},
	}
	for name, build := range mk {
		t.Run(name, func(t *testing.T) {
			im := build(t)
			if _, err := im.get("", "OK_KEY"); err == nil {
				t.Error("empty workspace must error")
			}
			if _, err := im.get("w1", ""); err == nil {
				t.Error("empty env var must error")
			}
			if v, err := im.get("w1", "NOT_IN_MANIFESTS"); err == nil || v != "" || !errors.Is(err, ErrUnknownCredential) {
				t.Errorf("unknown: got (%q,%v)", v, err)
			}
			if v, err := im.get("w1", "OK_KEY"); err != nil || v != "" {
				t.Errorf("unset: got (%q,%v)", v, err)
			}
			im.set("w1", "OK_KEY", "val-1")
			// empty was not cached, so the new value is visible at once
			if v, err := im.get("w1", "OK_KEY"); err != nil || v != "val-1" {
				t.Errorf("after set: got (%q,%v)", v, err)
			}
			im.set("w1", "OK_KEY", "val-2")
			if v, _ := im.get("w1", "OK_KEY"); v != "val-1" {
				t.Errorf("non-empty must be cached within TTL, got %q", v)
			}
			im.adv(TTL + time.Second)
			if v, _ := im.get("w1", "OK_KEY"); v != "val-2" {
				t.Errorf("cache must expire after TTL, got %q", v)
			}
		})
	}
}

func TestLocalRejectsEnvVarNotInManifests(t *testing.T) {
	st := newMem()
	st.set("", "SECRET_X", "plain")
	l, _ := newLocalTest(t, st, map[string]string{"SECRET_X": "envval"}, allowOnly("OTHER"))
	v, err := l.Get(context.Background(), "w1", "SECRET_X")
	if v != "" || !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("got (%q,%v)", v, err)
	}
	if st.reads != 0 {
		t.Fatal("a disallowed var must not touch the store")
	}
}

func TestLocalWorkspaceSecretBeatsGlobal(t *testing.T) {
	st := newMem()
	env := testEnvelope(t)
	ctx := context.Background()
	ct, _ := env.Encrypt(ctx, "ws-value")
	gct, _ := env.Encrypt(ctx, "global-value")
	st.set("", "K", gct)
	l, _ := newLocalTest(t, st, map[string]string{"K": "env-value"}, allowOnly("K"))
	if v, err := l.Get(ctx, "w2", "K"); err != nil || v != "global-value" {
		t.Fatalf("encrypted global: got (%q,%v)", v, err)
	}
	st.set("w1", "K", ct)
	if v, err := l.Get(ctx, "w1", "K"); err != nil || v != "ws-value" {
		t.Fatalf("workspace: got (%q,%v)", v, err)
	}
}

func TestLocalCachesNonEmptyOnly(t *testing.T) {
	st := newMem()
	l, clk := newLocalTest(t, st, nil, allowOnly("K"))
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if v, err := l.Get(ctx, "w1", "K"); v != "" || err != nil {
			t.Fatal(v, err)
		}
	}
	if st.reads != 4 { // 2 lookups x (workspace + global) : empty never cached
		t.Fatalf("empty must not be cached, reads=%d", st.reads)
	}
	st.set("w1", "K", "plain-legacy")
	l.Get(ctx, "w1", "K")
	n := st.reads
	l.Get(ctx, "w1", "K")
	if st.reads != n {
		t.Fatal("non-empty must be served from cache")
	}
	clk.Add(TTL)
	l.Get(ctx, "w1", "K")
	if st.reads == n {
		t.Fatal("cache must expire at TTL")
	}
	// errors are not cached either
	st2 := newMem()
	st2.err = errors.New("db down")
	l2, _ := newLocalTest(t, st2, nil, allowOnly("K"))
	if _, err := l2.Get(ctx, "w1", "K"); err == nil {
		t.Fatal("store error must surface")
	}
	st2.err = nil
	st2.set("w1", "K", "now")
	if v, err := l2.Get(ctx, "w1", "K"); err != nil || v != "now" {
		t.Fatalf("got (%q,%v)", v, err)
	}
}

func TestLocalErrorsHaveNoSecret(t *testing.T) {
	const secret = "sk-SUPER-SECRET-VALUE"
	ctx := context.Background()
	other, err := secrets.NewEnvelope(secrets.Config{MasterKey: "a-different-master-key", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := other.Encrypt(ctx, secret)
	st := newMem()
	st.set("w1", "K", ct) // encrypted under another key: undecryptable
	l, _ := newLocalTest(t, st, map[string]string{"K": "ENV-FALLBACK-VALUE"}, allowOnly("K"))
	v, gerr := l.Get(ctx, "w1", "K")
	if gerr == nil {
		t.Fatal("decrypt failure must be an error, not an env fallback")
	}
	if v != "" {
		t.Fatalf("value must be empty on error, got %q", v)
	}
	for _, leak := range []string{secret, ct, "ENV-FALLBACK-VALUE"} {
		if strings.Contains(gerr.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, gerr)
		}
	}
	// store failures carry no values either
	st2 := newMem()
	st2.err = errors.New("connection refused")
	l2, _ := newLocalTest(t, st2, map[string]string{"K": secret}, allowOnly("K"))
	if _, gerr := l2.Get(ctx, "w1", "K"); gerr == nil || strings.Contains(gerr.Error(), secret) {
		t.Fatalf("bad error: %v", gerr)
	}
}

func TestLocalIsolatesWorkspaces(t *testing.T) {
	st := newMem()
	st.set("w1", "K", "one")
	st.set("w2", "K", "two")
	l, _ := newLocalTest(t, st, nil, allowOnly("K"))
	ctx := context.Background()
	for i := 0; i < 2; i++ { // second round is served from cache
		if v, _ := l.Get(ctx, "w1", "K"); v != "one" {
			t.Fatalf("w1 got %q", v)
		}
		if v, _ := l.Get(ctx, "w2", "K"); v != "two" {
			t.Fatalf("w2 got %q", v)
		}
	}
	if v, _ := l.Get(ctx, "w3", "K"); v != "" {
		t.Fatalf("w3 must see nothing, got %q", v)
	}
}

func TestManifestAllowObservesEnvVars(t *testing.T) {
	a := NewManifestAllow(fakeManifests{"p1": `{"auth":{"type":"bearer","env_var":"P1_KEY"}}`, "p2": `{"auth":{"type":"none"}}`})
	if a.Allowed("P1_KEY") {
		t.Fatal("nothing observed yet")
	}
	if _, _, err := a.Manifest(context.Background(), "p1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Manifest(context.Background(), "p2"); err != nil {
		t.Fatal(err)
	}
	if !a.Allowed("P1_KEY") || a.Allowed("") || a.Allowed("OTHER") {
		t.Fatal("allow set wrong")
	}
	if _, _, err := a.Manifest(context.Background(), "missing"); err == nil {
		t.Fatal("source error must propagate")
	}
}

type fakeManifests map[string]string

func (f fakeManifests) Manifest(_ context.Context, name string) (json.RawMessage, string, error) {
	m, ok := f[name]
	if !ok {
		return nil, "", ErrManifestNotFound
	}
	return json.RawMessage(m), "sha", nil
}
