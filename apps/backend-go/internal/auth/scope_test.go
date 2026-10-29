package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var scopeSigner = Signer{Secret: []byte("scope-secret"), AccessTTL: time.Hour, RefreshTTL: 2 * time.Hour}

func payload(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// SSO sessions are workspace-scoped: the sso_ws claim names the only workspace the token is valid for.
func TestWorkspaceScopedTokens(t *testing.T) {
	acc, err := scopeSigner.AccessFor("alice", []string{"sso"}, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if p := payload(t, acc); p[WorkspaceClaim] != "ws-1" {
		t.Fatalf("payload %v", p)
	}
	c, err := scopeSigner.ParseAccess(acc)
	if err != nil || c.Workspace != "ws-1" || c.Type != "access" || !c.AllowsWorkspace("ws-1") || c.AllowsWorkspace("ws-2") || c.AllowsWorkspace("") {
		t.Fatalf("%+v %v", c, err)
	}
	ref, _ := scopeSigner.RefreshFor("alice", []string{"sso"}, "ws-1")
	if c, err := scopeSigner.ParseAny(ref); err != nil || c.Workspace != "ws-1" || c.Type != "refresh" {
		t.Fatalf("refresh %+v %v", c, err)
	}
}

// Password tokens carry no scope claim at all (wire-identical to before) and allow any workspace.
func TestUnscopedTokensUnchanged(t *testing.T) {
	acc, _ := scopeSigner.Access("alice", []string{"pwd"})
	if _, ok := payload(t, acc)[WorkspaceClaim]; ok {
		t.Fatal("password token carries sso_ws")
	}
	c, err := scopeSigner.ParseAccess(acc)
	if err != nil || c.Workspace != "" || !c.AllowsWorkspace("any") {
		t.Fatalf("%+v %v", c, err)
	}
	if a, _ := scopeSigner.AccessFor("alice", nil, ""); payload(t, a)[WorkspaceClaim] != nil {
		t.Fatal("empty scope minted a claim")
	}
}

// A present but malformed scope claim fails closed (never read as "unscoped").
func TestMalformedWorkspaceClaimRejected(t *testing.T) {
	for _, v := range []any{"", 7, []any{"ws-1"}, nil, map[string]any{}} {
		tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": "alice", "type": "access", "exp": time.Now().Add(time.Hour).Unix(), WorkspaceClaim: v,
		}).SignedString(scopeSigner.Secret)
		if _, err := scopeSigner.ParseAny(tok); err == nil {
			t.Errorf("sso_ws=%#v accepted", v)
		}
	}
}
