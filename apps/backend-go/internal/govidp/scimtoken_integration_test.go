//go:build integration

package govidp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/api"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

func (e *env) rotate(t *testing.T, ws string, now time.Time) string {
	t.Helper()
	var tok string
	e.in(t, ws, func(tx pgx.Tx) error {
		var err error
		tok, err = Rotate(context.Background(), tx, ws, e.owner, now)
		return err
	})
	return tok
}

func (e *env) status(t *testing.T, ws string, now time.Time) *TokenStatus {
	t.Helper()
	var st *TokenStatus
	e.in(t, ws, func(tx pgx.Tx) error {
		var err error
		st, err = Status(context.Background(), tx, ws, now)
		return err
	})
	return st
}

func scimStatus(t *testing.T, e *env, slug, tok string) int {
	t.Helper()
	r := httpx.NewRouter()
	api.RegisterSCIM(r, api.Deps{Pool: e.pool, Identity: identity.NewStore(e.pool), Config: config.APIConfig{ScimTokenTTLDays: 90}})
	req := httptest.NewRequest("GET", "/scim/v2/"+slug+"/ServiceProviderConfig", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func TestRotateShapeAndStoredHash(t *testing.T) {
	e := setup(t)
	now := time.Unix(1_700_000_000, 123_000_000)
	tok := e.rotate(t, e.ws, now)
	if !strings.HasPrefix(tok, "og_scim_") || len(tok) != 8+48 || strings.ContainsAny(tok, "=+/") {
		t.Fatalf("token shape: len %d", len(tok))
	}
	var hash, prefix string
	var created float64
	var lastUsed *float64
	var by *int32
	if err := e.pool.QueryRow(context.Background(), `SELECT token_hash, token_prefix, created_at, last_used_at, created_by FROM workspace_scim_tokens WHERE workspace_id=$1`, e.ws).Scan(&hash, &prefix, &created, &lastUsed, &by); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(tok))
	if hash != hex.EncodeToString(sum[:]) || prefix != tok[:16] || lastUsed != nil || by == nil || *by != e.owner {
		t.Fatal("stored row does not match python layout")
	}
	if created < 1_700_000_000.1 || created > 1_700_000_000.2 {
		t.Fatalf("created_at %v", created)
	}
	if strings.Contains(hash, tok) {
		t.Fatal("plaintext stored")
	}
}

func TestRotateInvalidatesPrevious(t *testing.T) {
	e := setup(t)
	now := time.Now()
	old := e.rotate(t, e.ws, now)
	if c := scimStatus(t, e, e.ws, old); c != http.StatusOK {
		t.Fatalf("fresh token should authenticate, got %d", c)
	}
	e.in(t, e.ws, func(tx pgx.Tx) error { // simulate a use, then rotate clears last_used_at
		_, err := tx.Exec(context.Background(), `UPDATE workspace_scim_tokens SET last_used_at=5 WHERE workspace_id=$1`, e.ws)
		return err
	})
	fresh := e.rotate(t, e.ws, now)
	if c := scimStatus(t, e, e.ws, old); c != http.StatusUnauthorized {
		t.Fatalf("old token still authenticates: %d", c)
	}
	if c := scimStatus(t, e, e.ws, fresh); c != http.StatusOK {
		t.Fatalf("new token rejected: %d", c)
	}
	if st := e.status(t, e.ws, now); st.LastUsedAt != nil && *st.LastUsedAt == 5 {
		t.Fatal("rotate must clear last_used_at")
	}
}

func TestRotateTTLClamp(t *testing.T) {
	e := setup(t)
	now := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		env  string
		days float64
	}{{"0", 1}, {"-7", 1}, {"400", 365}, {"abc", 90}, {"", 90}, {"30", 30}, {" 5 ", 5}} {
		t.Setenv("OPENGTM_SCIM_TOKEN_TTL_DAYS", c.env)
		e.rotate(t, e.ws, now)
		st := e.status(t, e.ws, now)
		if got := (st.ExpiresAt - st.CreatedAt) / 86400; got != c.days {
			t.Errorf("TTL %q: got %v days want %v", c.env, got, c.days)
		}
	}
}

func TestStatusExpired(t *testing.T) {
	e := setup(t)
	now := time.Unix(1_700_000_000, 0)
	if st := e.status(t, e.ws, now); st != nil {
		t.Fatal("no row means nil status")
	}
	e.rotate(t, e.ws, now)
	if st := e.status(t, e.ws, now.Add(89*24*time.Hour)); st.Expired {
		t.Fatal("not yet expired")
	}
	st := e.status(t, e.ws, now.Add(90*24*time.Hour)) // expires_at <= now
	if !st.Expired {
		t.Fatal("boundary is expired")
	}
	// NULL expires_at falls back to created_at + TTL (Python `expires_at or ...`).
	e.in(t, e.ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE workspace_scim_tokens SET expires_at=NULL WHERE workspace_id=$1`, e.ws)
		return err
	})
	st = e.status(t, e.ws, now)
	if st.ExpiresAt != st.CreatedAt+90*86400 || st.Expired {
		t.Fatalf("fallback: %+v", st)
	}
}

func TestRevoke(t *testing.T) {
	e := setup(t)
	now := time.Now()
	tok := e.rotate(t, e.ws, now)
	e.in(t, e.ws, func(tx pgx.Tx) error { return Revoke(context.Background(), tx, e.ws) })
	if e.status(t, e.ws, now) != nil {
		t.Fatal("status after revoke")
	}
	if c := scimStatus(t, e, e.ws, tok); c != http.StatusUnauthorized {
		t.Fatalf("revoked token authenticates: %d", c)
	}
	e.in(t, e.ws, func(tx pgx.Tx) error { return Revoke(context.Background(), tx, e.ws) }) // idempotent
}

func TestTenantIsolationSCIM(t *testing.T) {
	e := setup(t)
	now := time.Now()
	tokA := e.rotate(t, e.ws, now)
	if e.status(t, e.ws2, now) != nil {
		t.Fatal("status leaked across workspaces")
	}
	e.in(t, e.ws2, func(tx pgx.Tx) error { return Revoke(context.Background(), tx, e.ws2) })
	if e.status(t, e.ws, now) == nil {
		t.Fatal("revoke in B removed A's token")
	}
	if c := scimStatus(t, e, e.ws2, tokA); c != http.StatusUnauthorized {
		t.Fatalf("A's token works on B: %d", c)
	}
}

func TestTokenNeverLogged(t *testing.T) {
	e := setup(t)
	buf := capture(t)
	now := time.Now()
	tok := e.rotate(t, e.ws, now)
	e.status(t, e.ws, now)
	const sec = "client-secret-placeholder-q1"
	e.in(t, e.ws, func(tx pgx.Tx) error {
		_, err := SaveConfig(context.Background(), tx, e.res, e.ws, OIDCInput{Enabled: true, Issuer: "https://login.example.com", ClientID: "c", ClientSecret: sec, DefaultRole: "viewer"})
		return err
	})
	_ = e.try(e.ws, func(tx pgx.Tx) error { // failing path: error text must not carry secrets
		_, err := SaveConfig(context.Background(), tx, e.res, e.ws, OIDCInput{Issuer: "http://x", ClientSecret: sec})
		if err != nil && strings.Contains(err.Error(), sec) {
			t.Error("error leaks client secret")
		}
		return err
	})
	e.in(t, e.ws, func(tx pgx.Tx) error { return Revoke(context.Background(), tx, e.ws) })
	if strings.Contains(buf.String(), tok) || strings.Contains(buf.String(), tok[8:]) || strings.Contains(buf.String(), sec) {
		t.Fatal("secret material reached the log")
	}
}
