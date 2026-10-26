//go:build integration

package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type authEnv struct {
	pool *pgxpool.Pool
	d    Deps
	sfx  string
}

func sha(raw string) string { h := sha256.Sum256([]byte(raw)); return hex.EncodeToString(h[:]) }

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	e := &authEnv{pool: pool, sfx: sfx}
	e.d = Deps{
		Cfg: Config{RequireAuth: true},
		InWorkspace: func(ctx context.Context, ws string, fn func(pgx.Tx) error) error {
			return tenancy.InWorkspace(ctx, pool, ws, fn)
		},
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM mcp_tokens WHERE id LIKE $1`, "t5-%-"+sfx)
		_, _ = pool.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id LIKE $1`, "t5-%-"+sfx)
		_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id LIKE $1`, "t5-%-"+sfx)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE username LIKE $1`, "t5-%-"+sfx)
	})
	return e
}

func (e *authEnv) user(t *testing.T, name string) int32 {
	t.Helper()
	var id int32
	err := e.pool.QueryRow(context.Background(),
		`INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ($1,'x',true,false,'user') RETURNING id`,
		"t5-"+name+"-"+e.sfx).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *authEnv) workspace(t *testing.T, name string, owner *int32, slug string) string {
	t.Helper()
	id := "t5-" + name + "-" + e.sfx
	if slug == "" {
		slug = id
	}
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1::varchar,$1::text,$2::text,$3::int)`, id, slug, owner); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *authEnv) member(t *testing.T, ws string, uid int32, role string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,$3)
		 ON CONFLICT DO NOTHING`, ws, uid, role); err != nil {
		t.Fatal(err)
	}
}

func (e *authEnv) token(t *testing.T, alias, raw, ws string, uid int32, caps []string, expires, revoked *time.Time) string {
	t.Helper()
	id := "t5-" + alias + "-" + e.sfx
	cj, _ := json.Marshal(caps)
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO mcp_tokens (id, token_hash, prefix, name, user_id, workspace_id, capabilities, expires_at, revoked_at)
		 VALUES ($1,$2,'ycp_x','',$3,$4,$5::json,$6,$7)`,
		id, sha(raw), uid, ws, string(cj), expires, revoked); err != nil {
		t.Fatal(err)
	}
	return id
}

func wantAuthErr(t *testing.T, err error, msg string) {
	t.Helper()
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Msg != msg {
		t.Fatalf("want AuthError %q, got %v", msg, err)
	}
}

func TestResolveErrorStringsMatchPython(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	_, err := Resolve(ctx, e.d, "")
	wantAuthErr(t, err, "missing MCP token")
	_, err = Resolve(ctx, e.d, "ycp_no_such_token_"+e.sfx)
	wantAuthErr(t, err, "invalid MCP token")

	uid := e.user(t, "u1")
	ws := e.workspace(t, "ws1", &uid, "")
	raw := "ycp_ok_" + e.sfx
	id := e.token(t, "ok", raw, ws, uid, []string{"leads:read", "leads:write"}, nil, nil)
	c, err := Resolve(ctx, e.d, raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspaceID != ws || c.Slug != ws || c.TokenID != id || c.UserID == nil || *c.UserID != uid ||
		!c.Has("leads:read") || !c.Has("leads:write") || c.Has("leads:delete") {
		t.Fatalf("ctx = %+v", c)
	}
}

func TestResolveRevokedExpiredNonMember(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	uid := e.user(t, "u2")
	other := e.user(t, "u2other")
	ws := e.workspace(t, "ws2", &uid, "")
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	e.token(t, "revoked", "ycp_revoked_"+e.sfx, ws, uid, []string{"leads:read"}, nil, &past)
	_, err := Resolve(ctx, e.d, "ycp_revoked_"+e.sfx)
	wantAuthErr(t, err, "MCP token revoked")

	e.token(t, "expired", "ycp_expired_"+e.sfx, ws, uid, []string{"leads:read"}, &past, nil)
	_, err = Resolve(ctx, e.d, "ycp_expired_"+e.sfx)
	wantAuthErr(t, err, "MCP token expired")

	e.token(t, "future", "ycp_future_"+e.sfx, ws, uid, []string{"leads:read"}, &future, nil)
	if _, err = Resolve(ctx, e.d, "ycp_future_"+e.sfx); err != nil {
		t.Fatalf("unexpired token: %v", err)
	}

	// revoked wins over expired (Python order), both before membership.
	e.token(t, "both", "ycp_both_"+e.sfx, ws, uid, []string{"leads:read"}, &past, &past)
	_, err = Resolve(ctx, e.d, "ycp_both_"+e.sfx)
	wantAuthErr(t, err, "MCP token revoked")

	e.token(t, "removed", "ycp_removed_"+e.sfx, ws, other, []string{"leads:read"}, nil, nil)
	_, err = Resolve(ctx, e.d, "ycp_removed_"+e.sfx)
	wantAuthErr(t, err, "MCP token user is no longer a workspace member")
	// "MCP token workspace not found" is unreachable here: the workspace_members
	// foreign key means a live membership implies the workspace row (kept as a guard).
}

func TestResolveTouchesLastUsed(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	uid := e.user(t, "u3")
	ws := e.workspace(t, "ws4", &uid, "")
	raw := "ycp_touch_" + e.sfx
	id := e.token(t, "touch", raw, ws, uid, []string{"leads:read"}, nil, nil)
	var before *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT last_used_at FROM mcp_tokens WHERE id=$1`, id).Scan(&before); err != nil || before != nil {
		t.Fatalf("precondition: %v %v", before, err)
	}
	if _, err := Resolve(ctx, e.d, raw); err != nil {
		t.Fatal(err)
	}
	var after *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT last_used_at FROM mcp_tokens WHERE id=$1`, id).Scan(&after); err != nil || after == nil {
		t.Fatalf("last_used_at not touched: %v %v", after, err)
	}
	// Python commits the touch even when the membership re-check then fails.
	other := e.user(t, "u3other")
	id2 := e.token(t, "touch2", "ycp_touch2_"+e.sfx, ws, other, []string{"leads:read"}, nil, nil)
	if _, err := Resolve(ctx, e.d, "ycp_touch2_"+e.sfx); err == nil {
		t.Fatal("want non-member error")
	}
	var t2 *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT last_used_at FROM mcp_tokens WHERE id=$1`, id2).Scan(&t2); err != nil || t2 == nil {
		t.Fatalf("touch must persist before the membership check: %v %v", t2, err)
	}
}

func TestResolveKeylessReadOnlyMainWorkspace(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	d := e.d
	d.Cfg.RequireAuth = false

	// Active workspace from app_settings ACTIVE_WORKSPACE.
	ws := e.workspace(t, "active", nil, "")
	if _, err := e.pool.Exec(ctx, `INSERT INTO app_settings (key, value, updated_at) VALUES ('ACTIVE_WORKSPACE',$1,now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, ws); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM app_settings WHERE key='ACTIVE_WORKSPACE'`)
	})
	c, err := Resolve(ctx, d, "ignored-token")
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspaceID != ws || c.Slug != ws || c.UserID != nil || c.TokenID != "" || len(c.Capabilities) != 1 || !c.Has("leads:read") {
		t.Fatalf("keyless active ctx = %+v", c)
	}
	// An empty raw token is fine when auth is off.
	if _, err = Resolve(ctx, d, ""); err != nil {
		t.Fatal(err)
	}

	// No setting: the workspace whose slug is "main".
	if _, err = e.pool.Exec(ctx, `DELETE FROM app_settings WHERE key='ACTIVE_WORKSPACE'`); err != nil {
		t.Fatal(err)
	}
	var mainID string
	if err = e.pool.QueryRow(ctx, `SELECT id FROM workspaces WHERE slug='main'`).Scan(&mainID); err != nil {
		t.Skip("no main workspace in this database")
	}
	c, err = Resolve(ctx, d, "")
	if err != nil || c.WorkspaceID != mainID || c.Slug != "main" || c.UserID != nil || !c.Has("leads:read") || c.Has("leads:write") {
		t.Fatalf("keyless main ctx = %+v err=%v", c, err)
	}
}

func TestResolveTokenFromOtherWorkspaceScopedToItsOwn(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	uid := e.user(t, "u5")
	wsA := e.workspace(t, "wsA", &uid, "")
	wsB := e.workspace(t, "wsB", &uid, "")
	e.token(t, "tb", "ycp_tb_"+e.sfx, wsB, uid, []string{"leads:read"}, nil, nil)
	c, err := Resolve(ctx, e.d, "ycp_tb_"+e.sfx)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspaceID != wsB || c.WorkspaceID == wsA {
		t.Fatalf("token bound to B resolved to %s", c.WorkspaceID)
	}
	// Membership is per bound workspace: a member of A only, token minted for B.
	only := e.user(t, "u5only")
	e.member(t, wsA, only, "member")
	e.token(t, "tb2", "ycp_tb2_"+e.sfx, wsB, only, []string{"leads:read"}, nil, nil)
	_, err = Resolve(ctx, e.d, "ycp_tb2_"+e.sfx)
	wantAuthErr(t, err, "MCP token user is no longer a workspace member")
}

func TestRequireRoleOwnerAndDowngrade(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	owner := e.user(t, "own")
	ws := e.workspace(t, "wsr", &owner, "")
	e.member(t, ws, owner, "owner")
	roles := map[string]int32{}
	for _, r := range []string{"admin", "member", "editor", "viewer"} {
		roles[r] = e.user(t, "r-"+r)
		e.member(t, ws, roles[r], r)
	}
	ctxFor := func(uid int32) Ctx { return Ctx{WorkspaceID: ws, UserID: &uid} }
	if err := RequireRole(ctx, e.d, ctxFor(owner), AdminRoles...); err != nil {
		t.Fatalf("owner: %v", err)
	}
	for _, r := range []string{"admin", "member", "editor"} {
		if err := RequireRole(ctx, e.d, ctxFor(roles[r]), WriteRoles...); err != nil {
			t.Fatalf("%s write: %v", r, err)
		}
	}
	for _, r := range []string{"member", "editor", "viewer"} {
		wantAuthErr(t, RequireRole(ctx, e.d, ctxFor(roles[r]), AdminRoles...), "insufficient workspace role")
	}
	wantAuthErr(t, RequireRole(ctx, e.d, ctxFor(roles["viewer"]), WriteRoles...), "insufficient workspace role")

	// Live downgrade after the token was minted.
	if _, err := e.pool.Exec(ctx, `UPDATE workspace_members SET role='viewer' WHERE workspace_id=$1 AND user_id=$2`, ws, roles["admin"]); err != nil {
		t.Fatal(err)
	}
	wantAuthErr(t, RequireRole(ctx, e.d, ctxFor(roles["admin"]), WriteRoles...), "insufficient workspace role")

	// No member row at all (even as workspace owner_id): role "" fails, as Python's member_role.
	stranger := e.user(t, "stranger")
	wantAuthErr(t, RequireRole(ctx, e.d, ctxFor(stranger), WriteRoles...), "insufficient workspace role")
}

func TestRequireRoleVectorsAgainstDatabase(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	raw, err := os.ReadFile("testdata/roles.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Role    *string  `json:"role"`
		Roles   []string `json:"roles"`
		Allowed bool     `json:"allowed"`
		Error   *string  `json:"error"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	ws := e.workspace(t, "wsv", nil, "")
	for i, r := range rows {
		c := Ctx{WorkspaceID: ws}
		if r.Role == nil || !strings.HasPrefix(*r.Role, "<") {
			uid := e.user(t, fmt.Sprintf("v%d", i))
			if r.Role != nil {
				e.member(t, ws, uid, *r.Role)
			}
			c.UserID = &uid
		}
		err := RequireRole(ctx, e.d, c, r.Roles...)
		if r.Allowed != (err == nil) {
			t.Errorf("role %v roles %v: allowed=%v err=%v", r.Role, r.Roles, r.Allowed, err)
		}
		if err != nil && r.Error != nil {
			wantAuthErr(t, err, *r.Error)
		}
	}
}

func TestResolveNeverLogsToken(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	var buf bytes.Buffer
	oldW, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	oldSlog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { log.SetOutput(oldW); log.SetFlags(oldFlags); slog.SetDefault(oldSlog) })

	uid := e.user(t, "u6")
	ws := e.workspace(t, "wsl", &uid, "")
	past := time.Now().Add(-time.Hour)
	secret := "ycp_SECRETSECRETSECRET_" + e.sfx
	var errs []string
	e.token(t, "log-rev", secret, ws, uid, []string{"leads:read"}, nil, &past)
	_, err := Resolve(ctx, e.d, secret)
	errs = append(errs, fmt.Sprint(err))
	_, err = Resolve(ctx, e.d, secret+"x")
	errs = append(errs, fmt.Sprint(err))
	e.token(t, "log-ok", secret+"ok", ws, uid, []string{"leads:read"}, nil, nil)
	_, err = Resolve(ctx, e.d, secret+"ok")
	errs = append(errs, fmt.Sprint(err))
	for _, s := range append(errs, buf.String()) {
		if strings.Contains(s, "SECRETSECRET") {
			t.Fatalf("token leaked: %q", s)
		}
	}
}

func TestResolveIdentityTxPathAndFallback(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	uid := e.user(t, "u7")
	ws := e.workspace(t, "wsi", &uid, "")
	raw := "ycp_idtx_" + e.sfx
	e.token(t, "idtx", raw, ws, uid, []string{"leads:read"}, nil, nil)

	// IdentityTx wired: used, InWorkspace never called, no GUC set.
	d := e.d
	called, guc := 0, "unset"
	d.InWorkspace = func(context.Context, string, func(pgx.Tx) error) error {
		t.Fatal("InWorkspace must not be used when IdentityTx is set")
		return nil
	}
	d.IdentityTx = func(ctx context.Context, fn func(pgx.Tx) error) error {
		called++
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_ = tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.workspace_id', true), '')`).Scan(&guc)
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	c, err := Resolve(ctx, d, raw)
	if err != nil || c.WorkspaceID != ws || called != 1 || guc != "" {
		t.Fatalf("identity path: %+v err=%v called=%d guc=%q", c, err, called, guc)
	}
	d.Cfg.RequireAuth = false
	if _, err := Resolve(ctx, d, ""); err != nil || called != 2 {
		t.Fatalf("keyless identity path: %v called=%d", err, called)
	}

	// IdentityTx nil: falls back to InWorkspace.
	d2 := e.d
	fb := 0
	inner := d2.InWorkspace
	d2.InWorkspace = func(ctx context.Context, w string, fn func(pgx.Tx) error) error { fb++; return inner(ctx, w, fn) }
	if c, err := Resolve(ctx, d2, raw); err != nil || c.WorkspaceID != ws || fb != 1 {
		t.Fatalf("fallback: %+v err=%v fb=%d", c, err, fb)
	}
}
