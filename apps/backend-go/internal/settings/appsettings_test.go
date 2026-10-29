package settings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

type fakeStore struct {
	m       map[string]string
	readErr error
}

func newFake() *fakeStore { return &fakeStore{m: map[string]string{}} }

func (f *fakeStore) AppSetting(_ context.Context, k string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.m[k], nil
}
func (f *fakeStore) WorkspaceSetting(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeStore) SetAppSetting(_ context.Context, k, v string) error { f.m[k] = v; return nil }

func (f *fakeStore) SetAppSettings(_ context.Context, values map[string]string) error {
	for key, value := range values {
		f.m[key] = value
	}
	return nil
}

type vector struct {
	Name, MasterKeyRaw, Plaintext, Token string
}

func pyVector(t *testing.T, name string) vector {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/secrets/python_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct {
		Name         string `json:"name"`
		MasterKeyRaw string `json:"master_key_raw"`
		Plaintext    string `json:"plaintext"`
		Token        string `json:"token"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Name == name {
			return vector{v.Name, v.MasterKeyRaw, v.Plaintext, v.Token}
		}
	}
	t.Fatalf("vector %s missing", name)
	return vector{}
}

func testApp(t *testing.T, st *fakeStore, env map[string]string) (*App, vector) {
	t.Helper()
	v := pyVector(t, "valid_fernet_key")
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: v.MasterKeyRaw, AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &App{
		Store:  st,
		Env:    e,
		Secret: func(k string) bool { return strings.HasSuffix(k, "_API_KEY") },
		Getenv: func(k string) string { return env[k] },
	}, v
}

func TestEmptyStoredValueFallsToEnv(t *testing.T) {
	st := newFake()
	st.m["X_API_KEY"] = ""
	a, _ := testApp(t, st, map[string]string{"X_API_KEY": "from-env"})
	got, err := a.Get(context.Background(), "X_API_KEY", "dflt")
	if err != nil || got != "from-env" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, _ := a.Get(context.Background(), "NOPE", "dflt"); got != "dflt" {
		t.Fatalf("default: %q", got)
	}
}

func TestGetSwallowsReadErrorToEnv(t *testing.T) {
	st := newFake()
	st.readErr = errors.New("db down")
	a, _ := testApp(t, st, map[string]string{"X_API_KEY": "from-env"})
	if got, err := a.Get(context.Background(), "X_API_KEY", ""); err != nil || got != "from-env" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSetEncryptsSecretKeysOnly(t *testing.T) {
	st := newFake()
	a, _ := testApp(t, st, nil)
	ctx := context.Background()
	if err := a.Set(ctx, "X_API_KEY", "sk-live-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Set(ctx, "X_BASE_URL", "https://x.test"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.m["X_API_KEY"], "enc:v1:") || strings.Contains(st.m["X_API_KEY"], "sk-live-1") {
		t.Fatalf("secret not encrypted: %q", st.m["X_API_KEY"])
	}
	if st.m["X_BASE_URL"] != "https://x.test" {
		t.Fatalf("non-secret changed: %q", st.m["X_BASE_URL"])
	}
	if got, _ := a.Get(ctx, "X_API_KEY", ""); got != "sk-live-1" {
		t.Fatalf("round trip %q", got)
	}
}

func TestProviderKeyWrittenByPythonDecrypts(t *testing.T) {
	st := newFake()
	a, v := testApp(t, st, nil)
	st.m["X_API_KEY"] = "enc:v1:" + v.Token
	if got, err := a.Get(context.Background(), "X_API_KEY", ""); err != nil || got != v.Plaintext {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLegacyPlaintextProviderKeyStillReads(t *testing.T) {
	st := newFake()
	st.m["X_API_KEY"] = "sk-legacy-plain"
	a, _ := testApp(t, st, nil)
	if got, err := a.Get(context.Background(), "X_API_KEY", ""); err != nil || got != "sk-legacy-plain" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestGetDecryptFailureIsErrorNotCiphertext(t *testing.T) {
	st := newFake()
	st.m["X_API_KEY"] = "enc:v1:gAAAAAB-not-a-valid-token"
	a, _ := testApp(t, st, nil)
	got, err := a.Get(context.Background(), "X_API_KEY", "dflt")
	if err == nil || got != "" || strings.Contains(err.Error(), "gAAAAAB") {
		t.Fatalf("got %q, %v", got, err)
	}
}

type singleWriteStore struct {
	secrets.Store
	writes int
}

func (s *singleWriteStore) SetAppSetting(context.Context, string, string) error {
	s.writes++
	return nil
}

func TestSetManyRefusesNonAtomicStoreWithoutIndividualWrites(t *testing.T) {
	a, _ := testApp(t, newFake(), nil)
	store := &singleWriteStore{Store: a.Store}
	a.Store = store
	if err := a.SetMany(context.Background(), map[string]string{"MODEL": "new", "X_API_KEY": "synthetic-key"}); err == nil {
		t.Fatal("a store without atomic writes was accepted")
	}
	if store.writes != 0 {
		t.Fatal("batch update fell back to individual writes")
	}
}

func TestSetManyEncryptionFailureLeavesAllSettingsUnchanged(t *testing.T) {
	store := newFake()
	store.m["MODEL"] = "initial-model"
	a, _ := testApp(t, store, nil)
	envelope, err := secrets.NewEnvelope(secrets.Config{SecretKey: secrets.InsecureSecretKey, AppEnv: "prod"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Env = envelope
	if err := a.SetMany(context.Background(), map[string]string{"MODEL": "replacement-model", "X_API_KEY": "synthetic-key"}); err == nil {
		t.Fatal("encryption failure was not surfaced")
	}
	if store.m["MODEL"] != "initial-model" || len(store.m) != 1 {
		t.Fatal("encryption failure left partially written settings")
	}
}
