package secrets

import (
	"context"
	"errors"
	"testing"
)

type fakeStore struct {
	app    map[string]string
	ws     map[string]string // "ws/key"
	setErr error
}

func (s *fakeStore) AppSetting(_ context.Context, key string) (string, error) { return s.app[key], nil }
func (s *fakeStore) WorkspaceSetting(_ context.Context, ws, key string) (string, bool, error) {
	v, ok := s.ws[ws+"/"+key]
	return v, ok, nil
}
func (s *fakeStore) SetWorkspaceSetting(_ context.Context, ws, key, value string) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.ws[ws+"/"+key] = value
	return nil
}

func newTestResolver(t *testing.T, st *fakeStore, env map[string]string) (*Resolver, *Envelope) {
	t.Helper()
	e := localEnv(t, Config{MasterKey: "resolver-test", AppEnv: "dev"})
	return NewResolver(e, st, func(k string) string { return env[k] }), e
}

func TestResolverOrder(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{app: map[string]string{}, ws: map[string]string{}}
	r, e := newTestResolver(t, st, map[string]string{"K": "env-v"})
	check := func(wantV, wantSrc string) {
		t.Helper()
		v, src, err := r.Get(ctx, "w1", "K")
		if err != nil || v != wantV || src != wantSrc {
			t.Fatalf("got (%q,%q,%v) want (%q,%q)", v, src, err, wantV, wantSrc)
		}
	}
	check("env-v", "global")
	st.app["K"] = "global-v"
	check("global-v", "global")
	ct, _ := e.Encrypt(ctx, "ws-v")
	st.ws["w1/K"] = ct
	check("ws-v", "workspace")
	// other workspace does not see it
	if v, src, _ := r.Get(ctx, "w2", "K"); v != "global-v" || src != "global" {
		t.Fatalf("w2 got %q %q", v, src)
	}
	st2 := &fakeStore{app: map[string]string{}, ws: map[string]string{}}
	r2, _ := newTestResolver(t, st2, nil)
	if v, src, err := r2.Get(ctx, "w1", "NOPE"); err != nil || v != "" || src != "default" {
		t.Fatalf("default got %q %q %v", v, src, err)
	}
}

func TestResolverEmptyGlobalFallsToEnv(t *testing.T) {
	st := &fakeStore{app: map[string]string{"K": ""}, ws: map[string]string{"w/K": ""}}
	r, _ := newTestResolver(t, st, map[string]string{"K": "env-v"})
	v, src, err := r.Get(context.Background(), "w", "K")
	if err != nil || v != "env-v" || src != "global" {
		t.Fatalf("got %q %q %v", v, src, err)
	}
}

func TestResolverReacherURLNeverFromWorkspace(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{app: map[string]string{}, ws: map[string]string{}}
	r, e := newTestResolver(t, st, map[string]string{"REACHER_URL": "http://env-reacher"})
	ct, _ := e.Encrypt(ctx, "http://evil.internal")
	st.ws["w/REACHER_URL"] = ct
	v, src, err := r.Get(ctx, "w", "REACHER_URL")
	if err != nil || v != "http://env-reacher" || src != "global" {
		t.Fatalf("got %q %q %v", v, src, err)
	}
}

func TestResolverEmptyWorkspaceIDSkipsWorkspace(t *testing.T) {
	st := &fakeStore{app: map[string]string{"K": "g"}, ws: map[string]string{"/K": "x"}}
	r, _ := newTestResolver(t, st, nil)
	if v, src, _ := r.Get(context.Background(), "", "K"); v != "g" || src != "global" {
		t.Fatalf("%q %q", v, src)
	}
}

func TestResolverGlobalPlaintextPassthrough(t *testing.T) {
	st := &fakeStore{app: map[string]string{"K": "sk-legacy"}, ws: map[string]string{}}
	r, _ := newTestResolver(t, st, nil)
	if v, src, err := r.Get(context.Background(), "w", "K"); err != nil || v != "sk-legacy" || src != "global" {
		t.Fatalf("%q %q %v", v, src, err)
	}
}

func TestResolverDecryptsEncryptedGlobalKey(t *testing.T) {
	v := loadVectors(t)[0]
	st := &fakeStore{app: map[string]string{"K": EncPrefix + v.Token}, ws: map[string]string{}}
	e := localEnv(t, Config{MasterKey: v.Master, AppEnv: "dev"})
	r := NewResolver(e, st, func(string) string { return "" })
	got, src, err := r.Get(context.Background(), "w", "K")
	if err != nil || got != v.Plaintext || src != "global" {
		t.Fatalf("%q %q %v", got, src, err)
	}
}

func TestResolverWrongKeyErrorsNotCiphertext(t *testing.T) {
	v := loadVectors(t)[0]
	st := &fakeStore{app: map[string]string{"K": EncPrefix + v.Token}, ws: map[string]string{}}
	r, _ := newTestResolver(t, st, nil)
	got, _, err := r.Get(context.Background(), "w", "K")
	if err == nil || got != "" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestSetBlankIgnored(t *testing.T) {
	st := &fakeStore{app: map[string]string{}, ws: map[string]string{}}
	r, _ := newTestResolver(t, st, nil)
	for _, v := range []string{"", "   ", "\n\t"} {
		if err := r.Set(context.Background(), "w", "K", v); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.ws) != 0 {
		t.Fatal("blank value stored")
	}
	if err := r.Set(context.Background(), "", "K", "v"); err == nil {
		t.Fatal("empty workspace should error")
	}
}

func TestSetTrimsAndEncrypts(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{app: map[string]string{}, ws: map[string]string{}}
	r, _ := newTestResolver(t, st, nil)
	if err := r.Set(ctx, "w", "K", "  sk-secret \n"); err != nil {
		t.Fatal(err)
	}
	raw := st.ws["w/K"]
	if raw == "sk-secret" || len(raw) < len(EncPrefix) || raw[:len(EncPrefix)] != EncPrefix {
		t.Fatalf("raw = %q", raw)
	}
	if v, src, _ := r.Get(ctx, "w", "K"); v != "sk-secret" || src != "workspace" {
		t.Fatalf("%q %q", v, src)
	}
	if ok, err := r.Has(ctx, "w", "K"); !ok || err != nil {
		t.Fatal("Has")
	}
	if ok, _ := r.Has(ctx, "w", "other"); ok {
		t.Fatal("Has other")
	}
	st.setErr = errors.New("db down")
	if err := r.Set(ctx, "w", "K2", "v"); err == nil {
		t.Fatal("store error swallowed")
	}
}
