//go:build integration

package secrets_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const itWS = "ws-secrets-it"

func itSetup(t *testing.T, master string) (*secrets.Resolver, *pgxpool.Pool) {
	t.Helper()
	pool := testpg.NewAppPool(t, 2)
	ctx := context.Background()
	clean := func() {
		_ = tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM workspace_settings WHERE workspace_id = $1`, itWS)
			return err
		})
		_, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key LIKE 'IT\_%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id = ANY($1)`, []string{itWS, "ws-other"})
	}
	clean()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1::text,'it',$1::text)`, itWS); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clean)
	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: master, AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return secrets.NewResolver(env, secrets.NewPgStore(pool), func(string) string { return "" }), pool
}

func TestSetStoresCiphertextNotPlaintext(t *testing.T) {
	ctx := context.Background()
	r, pool := itSetup(t, "it-master")
	if err := r.Set(ctx, itWS, "IT_KEY", " sk-live-123 "); err != nil {
		t.Fatal(err)
	}
	if err := r.Set(ctx, itWS, "IT_KEY", "sk-live-456"); err != nil { // upsert
		t.Fatal(err)
	}
	var raw string
	if err := tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT value FROM workspace_settings WHERE workspace_id=$1 AND key='IT_KEY'`, itWS).Scan(&raw)
	}); err != nil {
		t.Fatal(err)
	}
	if raw == "sk-live-456" || len(raw) < 8 || raw[:7] != secrets.EncPrefix {
		t.Fatalf("raw row is not ciphertext: %.10q", raw)
	}
	v, src, err := r.Get(ctx, itWS, "IT_KEY")
	if err != nil || v != "sk-live-456" || src != "workspace" {
		t.Fatalf("%q %q %v", v, src, err)
	}
	if ok, err := r.Has(ctx, itWS, "IT_KEY"); err != nil || !ok {
		t.Fatal("Has")
	}
	// RLS: another workspace sees nothing.
	if ok, _ := r.Has(ctx, "ws-other", "IT_KEY"); ok {
		t.Fatal("cross-workspace leak")
	}
}

func TestResolverDecryptsRowWrittenByPython(t *testing.T) {
	ctx := context.Background()
	v := itVector(t)
	r, pool := itSetup(t, v.Master)
	_ = pool
	// Row inserted as the table owner (bypasses RLS), like a Python-written row.
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(ctx, `INSERT INTO workspace_settings (workspace_id,key,value) VALUES ($1,'IT_PY',$2)`, itWS, secrets.EncPrefix+v.Token); err != nil {
		t.Fatal(err)
	}
	got, src, err := r.Get(ctx, itWS, "IT_PY")
	if err != nil || got != v.Plaintext || src != "workspace" {
		t.Fatalf("%q %q %v", got, src, err)
	}
}

func TestResolverDecryptsEncryptedGlobalKey(t *testing.T) {
	ctx := context.Background()
	v := itVector(t)
	r, pool := itSetup(t, v.Master)
	if _, err := pool.Exec(ctx, `INSERT INTO app_settings (key,value) VALUES ('IT_GLOBAL',$1)`, secrets.EncPrefix+v.Token); err != nil {
		t.Fatal(err)
	}
	got, src, err := r.Get(ctx, itWS, "IT_GLOBAL")
	if err != nil || got != v.Plaintext || src != "global" {
		t.Fatalf("%q %q %v", got, src, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_settings (key,value) VALUES ('IT_PLAIN','sk-plain')`); err != nil {
		t.Fatal(err)
	}
	if got, src, _ := r.Get(ctx, itWS, "IT_PLAIN"); got != "sk-plain" || src != "global" {
		t.Fatalf("%q %q", got, src)
	}
}

type itVec struct{ Master, Plaintext, Token string }

func itVector(t *testing.T) itVec {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/secrets/python_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct {
		Master    string `json:"master_key_raw"`
		Plaintext string `json:"plaintext"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	return itVec{vs[0].Master, vs[0].Plaintext, vs[0].Token}
}
