//go:build integration

package tenancy_test

import (
	"context"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func TestIdentityPlaneNoRLS_TenantTablesForceRLS(t *testing.T) {
	ctx := context.Background()
	pool := testpg.NewAppPool(t, 1)
	noRLS := []string{
		"workspaces", "workspace_members", "user_active_workspace",
		"workspace_oidc_identities", "workspace_scim_tokens",
		"app_settings", "workbook_functions", "chat_memories",
	}
	forceRLS := []string{
		"workspace_settings", "workspace_member_permissions", "workspace_scim_users",
		"workspace_scim_groups", "workspace_scim_group_members",
		"chat_conversations", "chat_messages", "chat_tool_approvals",
	}
	state := map[string][2]bool{}
	rows, err := pool.Query(ctx, `SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname = ANY($1)`,
		append(append([]string{}, noRLS...), forceRLS...))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var on, force bool
		if err := rows.Scan(&name, &on, &force); err != nil {
			t.Fatal(err)
		}
		state[name] = [2]bool{on, force}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, n := range noRLS {
		s, ok := state[n]
		if !ok || s[0] || s[1] {
			bad = append(bad, "expected no RLS: "+n)
		}
	}
	for _, n := range forceRLS {
		s, ok := state[n]
		if !ok || !s[0] || !s[1] {
			bad = append(bad, "expected ENABLE+FORCE RLS: "+n)
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

func TestWorkspaceSettingsIsolatedByTenant(t *testing.T) {
	ctx := context.Background()
	pool := testpg.NewAppPool(t, 1)
	wsA, wsB := "ws-a-"+t.Name(), "ws-b-"+t.Name()
	key := "k-" + t.Name()
	for _, ws := range []string{wsA, wsB} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1::varchar, $1::text, $1::text) ON CONFLICT DO NOTHING`, ws); err != nil {
			t.Fatalf("seed workspace %s: %v", ws, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id = ANY($1)`, []string{wsA, wsB})
		for _, ws := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM workspace_settings WHERE key = $1`, key)
				return err
			})
		}
	})
	if err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ($1, $2, 'secret-a')`, wsA, key)
		return err
	}); err != nil {
		t.Fatalf("insert as A: %v", err)
	}
	count := func(ws string) (n int) {
		if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM workspace_settings WHERE key = $1`, key).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if got := count(wsA); got != 1 {
		t.Fatalf("A sees %d rows, want 1", got)
	}
	if got := count(wsB); got != 0 {
		t.Fatalf("B sees %d rows, want 0", got)
	}
	// B cannot write a row for A.
	err := tenancy.InWorkspace(ctx, pool, wsB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ($1, $2, 'x')`, wsA, key+"-x")
		return err
	})
	if err == nil {
		t.Fatal("cross-tenant insert unexpectedly succeeded")
	}
}
