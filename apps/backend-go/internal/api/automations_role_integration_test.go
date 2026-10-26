//go:build integration

package api

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// TestRuntimeRoleCanUseMirrorsAndLedgers: the runtime (app login) role holds SELECT, INSERT, UPDATE
// and DELETE on the automation and watch tables, and the two cross-tenant mirrors
// (scheduled_triggers, watch_schedules) are NOT row-level-secured (the bootstrap reads them
// without a workspace) while every tenant table is.
func TestRuntimeRoleCanUseMirrorsAndLedgers(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	ctx := context.Background()
	mirrors := map[string]bool{"scheduled_triggers": true, "watch_schedules": true}
	for _, tbl := range []string{"scheduled_triggers", "watch_schedules", "trigger_runs", "trigger_action_results",
		"trigger_cap_reservations", "poll_budget_ledger", "watch_subscriptions", "automation_rules"} {
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, "public."+tbl, priv).Scan(&ok); err != nil {
				t.Fatalf("%s %s: %v", tbl, priv, err)
			}
			if !ok {
				t.Errorf("runtime role lacks %s on %s", priv, tbl)
			}
		}
		var rls bool
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity FROM pg_class WHERE oid = $1::regclass`, "public."+tbl).Scan(&rls); err != nil {
			t.Fatalf("%s rls: %v", tbl, err)
		}
		if rls == mirrors[tbl] {
			t.Errorf("%s: relrowsecurity=%v, want %v", tbl, rls, !mirrors[tbl])
		}
	}
	// A mirror write cycle without any workspace setting (rolled back).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM scheduled_triggers`).Scan(&n); err != nil {
		t.Fatalf("select scheduled_triggers: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM watch_schedules`).Scan(&n); err != nil {
		t.Fatalf("select watch_schedules: %v", err)
	}
}
