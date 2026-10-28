//go:build integration

package copilotauth

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Final security M5: enforce_sso applies to the copilot routes like every other workspace route: a non-owner with a
// password session is refused; an SSO session and the owner are let in; a workspace without the policy is unchanged.
func TestEnforceSSORefusesPasswordSession(t *testing.T) {
	e := newEnv(t)
	owner := e.user(t, "t15sso-owner", true)
	pw := e.user(t, "t15sso-member", true)
	e.workspace(t, "t15sso-ws", "t15sso-slug", &owner)
	e.member(t, "t15sso-ws", pw)
	e.exec(t, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,'owner')`, "t15sso-ws", owner.ID)

	// No policy yet: the password session resolves.
	if got, err := Resolve(e.request("t15sso-member", "t15sso-ws"), e.deps(true)); err != nil || got.WorkspaceID != "t15sso-ws" {
		t.Fatalf("no policy: %+v %v", got, err)
	}

	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, "t15sso-ws", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_settings (workspace_id,key,value) VALUES ($1,'OIDC_CONFIG','{"enforce_sso": true}')`, "t15sso-ws")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(ctx, e.pool, "t15sso-ws", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM workspace_settings WHERE workspace_id=$1`, "t15sso-ws")
			return err
		})
	})

	_, err := Resolve(e.request("t15sso-member", "t15sso-ws"), e.deps(true))
	wantErr(t, err, 403, "This workspace requires SSO. Sign in with your organization identity.")

	tok, _ := testSigner.AccessFor("t15sso-member", []string{"sso"}, "t15sso-ws")
	if got, err := Resolve(req("Bearer "+tok), e.deps(true)); err != nil || got.WorkspaceID != "t15sso-ws" {
		t.Fatalf("sso session: %+v %v", got, err)
	}
	if got, err := Resolve(e.request("t15sso-owner", "t15sso-ws"), e.deps(true)); err != nil || got.WorkspaceID != "t15sso-ws" {
		t.Fatalf("owner: %+v %v", got, err)
	}
}
