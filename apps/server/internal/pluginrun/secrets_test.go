package pluginrun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

const exampleProvider = "../../../../plugins/examples/declarative-provider"

type fakeStore struct {
	values map[string]string // "workspace/name"
	err    error
	calls  []string
}

func (f *fakeStore) WorkspaceSecret(_ context.Context, ws, name string) (string, bool, error) {
	f.calls = append(f.calls, ws+"/"+name)
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.values[ws+"/"+name]
	return v, ok, nil
}

func loadExample(t *testing.T) *manifest.Plugin {
	t.Helper()
	p, err := manifest.Load(exampleProvider)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasSecret("ACME_DATA_API_KEY") {
		t.Fatal("example provider no longer declares ACME_DATA_API_KEY")
	}
	return p
}

func TestSecretResolutionOrder(t *testing.T) {
	p := loadExample(t)
	t.Setenv("ACME_DATA_API_KEY", "from-environment")
	t.Setenv("UNDECLARED_SECRET", "from-environment-too")
	store := &fakeStore{values: map[string]string{
		"ws-a/ACME_DATA_API_KEY":   "from-workspace-a",
		"ws-a/UNDECLARED_SECRET":   "workspace-undeclared",
		"ws-b/OTHER_DECLARED_NAME": "irrelevant",
	}}
	r := &Runner{}
	r.SetSecretStore(store)
	ctx := context.Background()
	get := func(ws, name string) (string, error) {
		return r.secretsFor(WithWorkspace(ctx, ws), p).Secret(ctx, name)
	}

	if v, err := get("ws-a", "ACME_DATA_API_KEY"); err != nil || v != "from-workspace-a" {
		t.Fatalf("workspace secret must win: %q %v", v, err)
	}
	if v, err := get("ws-b", "ACME_DATA_API_KEY"); err != nil || v != "from-environment" {
		t.Fatalf("a workspace without its own secret falls back to the environment: %q %v", v, err)
	}
	// Undeclared names never resolve, from either layer, and are not even looked up.
	store.calls = nil
	if v, err := get("ws-a", "UNDECLARED_SECRET"); err != nil || v != "" {
		t.Fatalf("undeclared secret resolved: %q %v", v, err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("undeclared name reached the store: %v", store.calls)
	}
	// No workspace in the context (CLI, tests): environment only, store untouched.
	store.calls = nil
	v, err := r.secretsFor(ctx, p).Secret(ctx, "ACME_DATA_API_KEY")
	if err != nil || v != "from-environment" || len(store.calls) != 0 {
		t.Fatalf("no workspace: %q %v %v", v, err, store.calls)
	}
	// Without a store the Runner behaves exactly as before.
	plain := &Runner{}
	if v, _ := plain.secretsFor(WithWorkspace(ctx, "ws-a"), p).Secret(ctx, "ACME_DATA_API_KEY"); v != "from-environment" {
		t.Fatalf("no store: %q", v)
	}
}

func TestSecretStoreFailureFailsClosed(t *testing.T) {
	p := loadExample(t)
	t.Setenv("ACME_DATA_API_KEY", "from-environment")
	r := &Runner{}
	r.SetSecretStore(&fakeStore{err: errors.New("decrypt: wrong key")})
	ctx := WithWorkspace(context.Background(), "ws-a")
	v, err := r.secretsFor(ctx, p).Secret(ctx, "ACME_DATA_API_KEY")
	if err == nil || v != "" {
		t.Fatalf("a failing store must not fall back to the environment: %q %v", v, err)
	}
	if strings.Contains(err.Error(), "from-environment") {
		t.Fatal("error leaks the environment value")
	}
}
