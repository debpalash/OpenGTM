//go:build integration

package govidp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool    *pgxpool.Pool
	res     *secrets.Resolver
	ws, ws2 string
	owner   int32
}

var ctr int

func setup(t *testing.T) *env {
	t.Helper()
	p := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	ctr++
	tag := fmt.Sprintf("gi%d-%d", ctr, os.Getpid()%10000)
	e := &env{pool: p, ws: "ws-" + tag, ws2: "ws-" + tag + "-b"}
	if err := p.QueryRow(ctx, `INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ($1,'x',true,false,'user') RETURNING id`, tag+"-owner").Scan(&e.owner); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{e.ws, e.ws2} {
		if _, err := p.Exec(ctx, `INSERT INTO workspaces (id,name,slug,description,icon,owner_id,created_at,updated_at) VALUES ($1::text,$1::text,$1::text,'','',$2,1,1)`, w, e.owner); err != nil {
			t.Fatal(err)
		}
	}
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "govidp-it-master", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.res = secrets.NewResolver(envl, secrets.NewPgStore(p), func(string) string { return "" })
	t.Setenv("SSO_ALLOWED_ISSUER_HOSTS", "login.example.com,idp.corp.io")
	t.Cleanup(func() {
		for _, w := range []string{e.ws, e.ws2} {
			_ = tenancy.InWorkspace(ctx, p, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(ctx, `DELETE FROM workspace_settings WHERE workspace_id=$1`, w)
				return nil
			})
			_, _ = p.Exec(ctx, `DELETE FROM workspace_scim_tokens WHERE workspace_id=$1`, w)
			_, _ = p.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, w)
		}
		_, _ = p.Exec(ctx, `DELETE FROM users WHERE id=$1`, e.owner)
	})
	return e
}

func (e *env) in(t *testing.T, ws string, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), e.pool, ws, fn); err != nil {
		t.Fatal(err)
	}
}

// try runs fn in a tx and returns its error (no t.Fatal), rolling back on error.
func (e *env) try(ws string, fn func(tx pgx.Tx) error) error {
	return tenancy.InWorkspace(context.Background(), e.pool, ws, fn)
}

func (e *env) storedConfig(t *testing.T, ws string) (string, bool) {
	t.Helper()
	var raw string
	var found bool
	e.in(t, ws, func(tx pgx.Tx) error {
		err := tx.QueryRow(context.Background(), `SELECT value FROM workspace_settings WHERE workspace_id=$1 AND key='OIDC_CONFIG'`, ws).Scan(&raw)
		found = err == nil
		if err == pgx.ErrNoRows {
			return nil
		}
		return err
	})
	return raw, found
}

func inputFrom(m map[string]any) OIDCInput {
	b, _ := json.Marshal(m)
	var in OIDCInput
	_ = json.Unmarshal(b, &in)
	return in
}

func TestSaveConfigVectorsMatchPython(t *testing.T) {
	v := loadVectors(t)
	ctx := context.Background()
	for _, c := range v.Saves {
		t.Run(c.Name, func(t *testing.T) {
			e := setup(t)
			t.Setenv("SSO_ALLOWED_ISSUER_HOSTS", v.AllowedHosts)
			if c.ExistingSecret {
				if err := e.res.Set(ctx, e.ws, "OIDC_CLIENT_SECRET", "pre-existing-placeholder"); err != nil {
					t.Fatal(err)
				}
			}
			var got OIDCConfig
			err := e.try(e.ws, func(tx pgx.Tx) error {
				var err error
				got, err = SaveConfig(ctx, tx, e.res, e.ws, inputFrom(c.Input))
				return err
			})
			raw, found := e.storedConfig(t, e.ws)
			if c.Error != "" {
				if err == nil || err.Error() != c.Error {
					t.Fatalf("want error %q got %v", c.Error, err)
				}
				if found {
					t.Fatalf("rejected save must not store config")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if *c.Stored != raw {
				t.Fatalf("stored bytes differ:\n py %s\n go %s", *c.Stored, raw)
			}
			want, _ := json.Marshal(c.Config)
			have, _ := json.Marshal(got)
			if !bytes.Equal(want, have) {
				t.Fatalf("returned config differs:\n py %s\n go %s", want, have)
			}
		})
	}
}

func TestSaveConfigErrorStrings(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	cases := []struct {
		name string
		in   OIDCInput
		want string
	}{
		{"scheme", OIDCInput{Issuer: "http://login.example.com"}, "OIDC issuer must be a clean HTTPS origin or path"},
		{"host", OIDCInput{Issuer: "https://evil.com"}, "OIDC issuer host is not approved by SSO_ALLOWED_ISSUER_HOSTS"},
		{"role", OIDCInput{DefaultRole: "admin"}, "OIDC default role must be viewer, member, or editor"},
		{"enforce", OIDCInput{EnforceSSO: true, DefaultRole: "viewer"}, "SSO enforcement requires OIDC to be enabled"},
		{"enabled", OIDCInput{Enabled: true, DefaultRole: "viewer"}, "Enabled OIDC requires issuer, client_id, and client_secret"},
	}
	for _, c := range cases {
		err := e.try(e.ws, func(tx pgx.Tx) error { _, err := SaveConfig(ctx, tx, e.res, e.ws, c.in); return err })
		var ve *ValueError
		if err == nil || err.Error() != c.want || !errors.As(err, &ve) {
			t.Errorf("%s: want ValueError %q, got %T %v", c.name, c.want, err, err)
		}
	}
}

func TestSaveConfigDoesNotWriteSecretWhenBlank(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	in := OIDCInput{Issuer: "https://login.example.com", ClientID: "c", ClientSecret: "  \t ", DefaultRole: "viewer"}
	e.in(t, e.ws, func(tx pgx.Tx) error { _, err := SaveConfig(ctx, tx, e.res, e.ws, in); return err })
	if SecretConfigured(ctx, e.res, e.ws) {
		t.Fatal("blank secret must not be stored")
	}
	if err := e.res.Set(ctx, e.ws, "OIDC_CLIENT_SECRET", "kept-placeholder"); err != nil {
		t.Fatal(err)
	}
	in.ClientSecret = ""
	in.Enabled = true
	e.in(t, e.ws, func(tx pgx.Tx) error { _, err := SaveConfig(ctx, tx, e.res, e.ws, in); return err })
	got, _, _ := e.res.Get(ctx, e.ws, "OIDC_CLIENT_SECRET")
	if got != "kept-placeholder" {
		t.Fatal("blank client_secret clobbered the saved secret")
	}
	if !SecretConfigured(ctx, e.res, e.ws) {
		t.Fatal("secret should report configured")
	}
}

func TestSaveConfigNeverEchoesSecret(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	const sec = "very-secret-placeholder-xyz"
	var cfg OIDCConfig
	e.in(t, e.ws, func(tx pgx.Tx) error {
		var err error
		cfg, err = SaveConfig(ctx, tx, e.res, e.ws, OIDCInput{Enabled: true, Issuer: "https://login.example.com", ClientID: "c", ClientSecret: sec, DefaultRole: "viewer"})
		return err
	})
	out, _ := json.Marshal(cfg)
	raw, _ := e.storedConfig(t, e.ws)
	if strings.Contains(string(out), sec) || strings.Contains(raw, sec) {
		t.Fatal("client secret leaked into config")
	}
	var stored string
	e.in(t, e.ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT value FROM workspace_settings WHERE key='OIDC_CLIENT_SECRET' AND workspace_id=$1`, e.ws).Scan(&stored)
	})
	if stored == sec || !strings.HasPrefix(stored, secrets.EncPrefix) {
		t.Fatal("client secret must be stored encrypted")
	}
}

// Python's /auth/sso/* reads OIDC_CONFIG with json.loads over DEFAULT_CONFIG: the
// stored text must be compact, Python key order, ASCII-only JSON.
func TestSaveConfigStoredBytesReadableByPythonShape(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.in(t, e.ws, func(tx pgx.Tx) error {
		_, err := SaveConfig(ctx, tx, e.res, e.ws, OIDCInput{AllowedDomains: []string{"Zed.com", "ünï.com"}, DefaultRole: "member"})
		return err
	})
	raw, _ := e.storedConfig(t, e.ws)
	want := `{"enabled":false,"enforce_sso":false,"issuer":"","client_id":"","allowed_domains":["zed.com","\u00fcn\u00ef.com"],"auto_provision":false,"default_role":"member"}`
	if raw != want {
		t.Fatalf("got %s", raw)
	}
	var cfg OIDCConfig
	e.in(t, e.ws, func(tx pgx.Tx) error { var err error; cfg, err = GetConfig(ctx, tx, e.ws); return err })
	if cfg.DefaultRole != "member" || len(cfg.AllowedDomains) != 2 || cfg.AllowedDomains[1] != "ünï.com" {
		t.Fatalf("round trip: %+v", cfg)
	}
}

func TestGetConfigDefaultsAndPartialStored(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var cfg OIDCConfig
	e.in(t, e.ws, func(tx pgx.Tx) error { var err error; cfg, err = GetConfig(ctx, tx, e.ws); return err })
	b, _ := json.Marshal(cfg)
	if string(b) != `{"enabled":false,"enforce_sso":false,"issuer":"","client_id":"","allowed_domains":[],"auto_provision":false,"default_role":"viewer"}` {
		t.Fatalf("defaults: %s", b)
	}
	e.in(t, e.ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_settings (workspace_id,key,value) VALUES ($1,'OIDC_CONFIG',$2)`, e.ws, `{"enabled":true,"issuer":"https://idp.corp.io"}`)
		return err
	})
	e.in(t, e.ws, func(tx pgx.Tx) error { var err error; cfg, err = GetConfig(ctx, tx, e.ws); return err })
	if !cfg.Enabled || cfg.Issuer != "https://idp.corp.io" || cfg.DefaultRole != "viewer" || cfg.AllowedDomains == nil {
		t.Fatalf("partial: %+v", cfg)
	}
}

func TestTenantIsolationOIDC(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.in(t, e.ws, func(tx pgx.Tx) error {
		_, err := SaveConfig(ctx, tx, e.res, e.ws, OIDCInput{Enabled: true, Issuer: "https://login.example.com", ClientID: "a", ClientSecret: "s-a", DefaultRole: "viewer"})
		return err
	})
	var cfg OIDCConfig
	e.in(t, e.ws2, func(tx pgx.Tx) error { var err error; cfg, err = GetConfig(ctx, tx, e.ws2); return err })
	if cfg.Enabled || cfg.ClientID != "" {
		t.Fatalf("leak across tenants: %+v", cfg)
	}
	if SecretConfigured(ctx, e.res, e.ws2) || !SecretConfigured(ctx, e.res, e.ws) {
		t.Fatal("secret flag must be per workspace")
	}
}

// capture swaps slog default and the std logger for a buffer.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}
