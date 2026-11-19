//go:build integration

package analytics

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const wsA, wsB = "ws-an-a", "ws-an-b"

func cleanFigures(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	for _, ws := range []string{wsA, wsB} {
		err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"workbook_enrichments", "workbook_spend_attempts", "workbooks", "leads", "legacy_record_links", "person_entities", "company_entities"} {
				if _, err := tx.Exec(ctx, "DELETE FROM "+tbl); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// seedFigures gives each workspace shared records, one workbook, today's cells, and spend. wsB gets a different
// shape so a leak between workspaces shows up in the counts.
func seedFigures(ctx context.Context, t *testing.T, pool *pgxpool.Pool, ws string, companies, people int, cellsOK, cellsErr int) {
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		stmts := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO company_entities (id, workspace_id, canonical_name, accepted_fields)
			  SELECT $1||'-c'||g, $1, 'Co '||g, CASE WHEN g=1 THEN '{"domain":"a.test"}'::jsonb ELSE '{}'::jsonb END FROM generate_series(1,$2::int) g`, []any{ws, companies}},
			{`INSERT INTO person_entities (id, workspace_id, full_name, accepted_fields)
			  SELECT $1||'-p'||g, $1, 'P '||g,
			    CASE WHEN g=1 THEN '{"email":"x@a.test","email_verify_status":"valid"}'::jsonb
			         WHEN g=2 THEN '{"email":"y@a.test"}'::jsonb
			         WHEN g=3 THEN '{"email":"n/a"}'::jsonb ELSE '{}'::jsonb END FROM generate_series(1,$2::int) g`, []any{ws, people}},
			{`INSERT INTO workbooks (id, name, workspace_id, status, last_run_at) VALUES ($1||'-wb','WB',$1,'failed', now())`, []any{ws}},
			{`INSERT INTO leads (id, workspace_id, company) SELECT (CASE WHEN $1='ws-an-a' THEN 900000 ELSE 910000 END)+g, $1, 'L' FROM generate_series(1,$2::int) g`, []any{ws, cellsOK + cellsErr}},
			{`INSERT INTO workbook_enrichments (workbook_id, lead_id, column_id, workspace_id, status, provider, updated_at)
			  SELECT $1||'-wb', (CASE WHEN $1='ws-an-a' THEN 900000 ELSE 910000 END)+g, 'col'||g, $1, CASE WHEN g<=$2::int THEN 'complete' ELSE 'error' END,
			         CASE WHEN g<=$2::int THEN CASE WHEN g%2=0 THEN 'apollo' ELSE 'hunter' END END, now()
			  FROM generate_series(1,$2::int+$3::int) g`, []any{ws, cellsOK, cellsErr}},
			{`INSERT INTO workbook_spend_attempts (id, workspace_id, workbook_id, run_id, row_identity, column_id, provider, attempt_key, contract_hash, status, reserved_microusd, settled_microusd, cost_basis, created_at, updated_at)
			  VALUES ($1||'-s1',$1,$1||'-wb','r','row','c','apollo','k1','h','settled',50000,40000,'{}',extract(epoch FROM now()),extract(epoch FROM now())),
			         ($1||'-s2',$1,$1||'-wb','r','row','c','apollo','k2','h','released',50000,NULL,'{}',extract(epoch FROM now()),extract(epoch FROM now()))`, []any{ws}},
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecordFiguresReadsSharedRecordsAndWorkbooks(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	cleanFigures(ctx, t, pool)
	t.Cleanup(func() { cleanFigures(context.Background(), t, pool) })
	seedFigures(ctx, t, pool, wsA, 3, 4, 4, 2)
	seedFigures(ctx, t, pool, wsB, 1, 1, 1, 0)

	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		o, err := Overview(ctx, tx)
		if err != nil {
			return err
		}
		// The 6 legacy leads share one company name, and the lead store projects it into company_entities: 3+1.
		r := o.Records
		if r.Companies != 4 || r.People != 4 || r.PeopleWithEmail != 2 || r.VerifiedEmails != 1 || r.CompaniesNeedingDomain != 3 {
			t.Errorf("records: %+v", r)
		}
		if r.EmailPct != Pct(2, 4) || r.VerifiedPct != Pct(1, 4) {
			t.Errorf("pct: %+v", r)
		}
		a := o.Activity
		if a.WindowDays != WindowDays || len(a.ByDay) != WindowDays || a.Workbooks != 1 || a.WorkbooksRun != 1 || a.FailedWorkbooks != 1 || a.RunningWorkbooks != 0 {
			t.Errorf("activity: %+v", a)
		}
		if a.CellsFilled != 4 || a.CellsNoData != 2 || a.ByDay[WindowDays-1].Filled != 4 || a.ByDay[WindowDays-1].NoData != 2 || a.FillRatePct != Pct(4, 6) {
			t.Errorf("cells: %+v", a)
		}
		if len(o.Providers) != 2 || o.Providers[0].CellsFilled != 2 || o.Providers[0].SharePct != Pct(2, 4) {
			t.Errorf("providers: %+v", o.Providers)
		}
		var apollo ProviderRow
		for _, p := range o.Providers {
			if p.Provider == "apollo" {
				apollo = p
			}
		}
		if apollo.PaidAttempts != 1 || apollo.SpendUSD != 0.04 {
			t.Errorf("apollo (only the settled attempt is spend): %+v", apollo)
		}
		if o.Spend.WindowUSD != 0.04 || o.Spend.TotalUSD != 0.04 || len(o.Spend.ByDay) != 1 {
			t.Errorf("spend: %+v", o.Spend)
		}
		if o.Legacy != (Legacy{Present: true, Leads: 6}) || o.TotalLeads != 6 {
			t.Errorf("legacy: %+v total %d", o.Legacy, o.TotalLeads)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The other workspace sees only its own rows.
	err = tenancy.InWorkspace(ctx, pool, wsB, func(tx pgx.Tx) error {
		o, err := Overview(ctx, tx)
		if err != nil {
			return err
		}
		if o.Records.Companies != 2 || o.Records.People != 1 || o.Activity.CellsFilled != 1 || o.Spend.TotalUSD != 0.04 {
			t.Errorf("ws-b: %+v %+v %+v", o.Records, o.Activity, o.Spend)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A workspace with no rows at all reports zeros and empty lists, and no legacy scope.
func TestRecordFiguresEmptyWorkspace(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	cleanFigures(ctx, t, pool)
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		o, err := Overview(ctx, tx)
		if err != nil {
			return err
		}
		if o.Records.Companies != 0 || o.Records.EmailPct != 0 || o.Activity.FillRatePct != 0 || len(o.Providers) != 0 ||
			o.Providers == nil || o.Spend.ByDay == nil || o.Legacy.Present {
			t.Errorf("empty: %+v", o)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
