//go:build integration

package records

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestImportPlanningAndTenantSettingsStayTransactionLocal(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	cfg := s.Pool.Config()
	cfg.MaxConns = 1 // every assertion borrows the actual import connection again
	cfg.ConnConfig.RuntimeParams["plan_cache_mode"] = "force_generic_plan"
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE opengtm_records_test_app`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s.Pool = pool
	assertRestored := func() {
		t.Helper()
		var mode, tenant string
		if err := pool.QueryRow(ctx, `SELECT current_setting('plan_cache_mode'),coalesce(current_setting('app.workspace_id',true),'')`).Scan(&mode, &tenant); err != nil {
			t.Fatal(err)
		}
		if mode != "force_generic_plan" || tenant != "" {
			t.Fatalf("import leaked session state: mode=%q tenant=%q", mode, tenant)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM company_entities`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unscoped runtime connection exposed %d records: %v", count, err)
		}
	}
	request := ImportRequest{Kind: Company, Source: "clay:planning", IdempotencyKey: "batch", CSV: "Row ID,Company Name,Company Domain\n001,Planning company,planning.test\n"}
	for _, preview := range []bool{true, false, false} {
		result, err := s.Import(ctx, ws, request, preview)
		if err != nil || result.Imported != 1 || result.Rejected != 0 {
			t.Fatalf("preview=%v: %+v %v", preview, result, err)
		}
		assertRestored()
	}
	request.CSV = "Row ID,Company Name,Company Domain\n002,Different company,different.test\n"
	if _, err := s.Import(ctx, ws, request, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload reused the same batch key: %v", err)
	}
	assertRestored()
}
