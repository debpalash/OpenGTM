//go:build integration

package entities

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func TestEntitiesFORCERLS(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	ctx := context.Background()
	for _, tbl := range []string{"company_entities", "entity_review_pairs", "entity_merge_log"} {
		var enabled, forced bool
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, tbl).Scan(&enabled, &forced); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Errorf("%s: rls=%v force=%v", tbl, enabled, forced)
		}
		// Without app.workspace_id the non-superuser app role must see and write nothing.
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: unscoped read n=%d err=%v", tbl, n, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO company_entities (id, workspace_id, canonical_name) VALUES ('rls-x','ws-x','X')`); err == nil {
		t.Error("unscoped insert into company_entities must be rejected by RLS")
		_, _ = pool.Exec(ctx, `DELETE FROM company_entities WHERE id='rls-x'`)
	}
}
