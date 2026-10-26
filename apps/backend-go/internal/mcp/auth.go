package mcp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// AuthError is an authentication or authorization refusal whose Msg is safe to
// show a client (the HTTP 401 detail / the stdio refusal reason).
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// identityPlaneWS opens the transaction for the token and identity-plane reads:
// mcp_tokens, workspaces, workspace_members and app_settings carry no RLS, so
// the value only has to be non-empty; it never names a tenant.
const identityPlaneWS = "mcp-identity-plane"

// now is replaceable by tests.
var now = time.Now

// HashToken is auth.hash_token: sha256 hex of the plaintext token. Go-issued
// (/api/mcp/tokens) and Python-issued tokens share this hash.
func HashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// BearerToken parses an Authorization header like Python's partition(" "): the
// scheme (case-insensitive) must be "bearer"; the rest after the first space is
// the token ("" when absent). ok is false for any other scheme.
func BearerToken(header string) (string, bool) {
	scheme, rest, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	return rest, true
}

func keylessCaps() map[string]bool {
	m := make(map[string]bool, len(ReadCaps))
	for k := range ReadCaps {
		m[k] = true
	}
	return m
}

// Resolve authenticates raw into a Ctx. With MCP_REQUIRE_AUTH off it returns the
// keyless read-only context. Otherwise it hashes raw, looks the hash up in
// mcp_tokens, checks revocation and expiry, touches last_used_at in the same
// transaction, then re-verifies live workspace membership and the slug. Every
// refusal is an *AuthError; a missing database is ErrUnavailable; a database
// failure is returned as is (callers mask it).
func Resolve(ctx context.Context, d Deps, raw string) (Ctx, error) {
	if !d.Cfg.RequireAuth {
		return resolveKeyless(ctx, d)
	}
	if raw == "" {
		return Ctx{}, &AuthError{Msg: "missing MCP token"}
	}
	hash := HashToken(raw)

	var out Ctx
	var refusal *AuthError
	err := d.RunInWorkspace(ctx, identityPlaneWS, func(tx pgx.Tx) error {
		q := gen.New(tx)
		tok, err := q.McpAuthTokenByHash(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			refusal = &AuthError{Msg: "invalid MCP token"}
			return nil
		}
		if err != nil {
			return err
		}
		// The row was selected by this hash; re-compare in constant time so a
		// collation or index quirk can never match a near hash.
		if subtle.ConstantTimeCompare([]byte(tok.TokenHash), []byte(hash)) != 1 {
			refusal = &AuthError{Msg: "invalid MCP token"}
			return nil
		}
		if tok.RevokedAt.Valid {
			refusal = &AuthError{Msg: "MCP token revoked"}
			return nil
		}
		if tok.ExpiresAt.Valid && !tok.ExpiresAt.Time.After(now()) {
			refusal = &AuthError{Msg: "MCP token expired"}
			return nil
		}
		var granted []string
		if len(tok.Capabilities) > 0 {
			if err := json.Unmarshal(tok.Capabilities, &granted); err != nil {
				granted = nil
			}
		}
		// Touch inside the same transaction, as Python; it persists even when
		// the membership re-check below refuses.
		if err := q.McpAuthTouchToken(ctx, tok.ID); err != nil {
			return err
		}
		member, err := q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: tok.WorkspaceID, UserID: tok.UserID})
		if err != nil {
			return err
		}
		if !member {
			refusal = &AuthError{Msg: "MCP token user is no longer a workspace member"}
			return nil
		}
		slug, err := q.McpAuthWorkspaceSlug(ctx, tok.WorkspaceID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && slug == "") {
			refusal = &AuthError{Msg: "MCP token workspace not found"}
			return nil
		}
		if err != nil {
			return err
		}
		caps := make(map[string]bool, len(granted))
		for _, c := range granted {
			caps[c] = true
		}
		uid := tok.UserID
		out = Ctx{WorkspaceID: tok.WorkspaceID, Slug: slug, Capabilities: caps, UserID: &uid, TokenID: tok.ID}
		return nil
	})
	if err != nil {
		return Ctx{}, err
	}
	if refusal != nil {
		return Ctx{}, refusal
	}
	return out, nil
}

// resolveKeyless is _self_host_ctx: the app-setting ACTIVE_WORKSPACE, else the
// workspace whose slug is "main"; read capabilities only; no user.
func resolveKeyless(ctx context.Context, d Deps) (Ctx, error) {
	var out Ctx
	err := d.RunInWorkspace(ctx, identityPlaneWS, func(tx pgx.Tx) error {
		q := gen.New(tx)
		ws, err := q.GetAppSetting(ctx, "ACTIVE_WORKSPACE")
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if ws == "" {
			ws, err = q.McpAuthMainWorkspaceID(ctx)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		slug, err := q.McpAuthWorkspaceSlug(ctx, ws)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if slug == "" {
			slug = "main"
		}
		out = Ctx{WorkspaceID: ws, Slug: slug, Capabilities: keylessCaps()}
		return nil
	})
	return out, err
}
