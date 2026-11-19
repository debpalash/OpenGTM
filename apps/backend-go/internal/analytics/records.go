package analytics

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The shared-records half of the analytics overview. The original fields describe Legacy Leads (the
// compatibility record from the combined company/contact model); the fields below describe what a workspace
// has now: shared Companies and People, and the workbook activity and spend that enrich them. They are added
// next to the legacy fields, never in place of them.
//
// Every query runs in the caller's tenancy.InWorkspace transaction, so row-level security applies, and also
// names the workspace explicitly (current_setting('app.workspace_id')). Nothing reads provider_stats: that
// ledger has no workspace column, so its hit rates would mix tenants.

// WindowDays is the look-back of the activity, provider and spend figures.
const WindowDays = 30

// RecordsSummary counts shared records. The email definitions match records.Store.Overview, which feeds the
// Overview page, so the two pages agree.
type RecordsSummary struct {
	Companies              int64 `json:"companies"`
	People                 int64 `json:"people"`
	PeopleWithEmail        int64 `json:"people_with_email"`
	VerifiedEmails         int64 `json:"verified_emails"`
	CompaniesNeedingDomain int64 `json:"companies_needing_domain"`
	EmailPct               any   `json:"email_pct"`
	VerifiedPct            any   `json:"verified_pct"`
}

// ActivityDay is one day of cells written by workbook runs: Filled got a value, NoData ran and found nothing.
type ActivityDay struct {
	Date   string `json:"date"`
	Filled int64  `json:"filled"`
	NoData int64  `json:"no_data"`
}

// Activity is workbook enrichment over the last WindowDays.
type Activity struct {
	WindowDays       int           `json:"window_days"`
	Workbooks        int64         `json:"workbooks"`
	WorkbooksRun     int64         `json:"workbooks_run"` // workbooks whose last run started in the window
	RunningWorkbooks int64         `json:"running_workbooks"`
	FailedWorkbooks  int64         `json:"failed_workbooks"`
	CellsFilled      int64         `json:"cells_filled"`
	CellsNoData      int64         `json:"cells_no_data"`
	FillRatePct      any           `json:"fill_rate_pct"`
	ByDay            []ActivityDay `json:"by_day"`
}

// ProviderRow is one provider's contribution in the window: the cells it filled, its share of all filled
// cells, and what the spend ledger settled for it. Providers that only ever fail are not listed, because
// workbook_enrichments records the provider of a produced value only.
type ProviderRow struct {
	Provider     string  `json:"provider"`
	CellsFilled  int64   `json:"cells_filled"`
	SharePct     any     `json:"share_pct"`
	PaidAttempts int64   `json:"paid_attempts"`
	SpendUSD     float64 `json:"spend_usd"`
}

// SpendDay is the settled spend of one day.
type SpendDay struct {
	Date string  `json:"date"`
	USD  float64 `json:"usd"`
}

// Spend is settled provider spend from workbook_spend_attempts, the per-attempt ledger.
type Spend struct {
	WindowDays int        `json:"window_days"`
	WindowUSD  float64    `json:"window_usd"`
	TotalUSD   float64    `json:"total_usd"`
	ByDay      []SpendDay `json:"by_day"`
}

// Legacy says whether the workspace has Legacy Lead rows, so the page can show the legacy scope only then.
type Legacy struct {
	Present bool  `json:"present"`
	Leads   int64 `json:"leads"`
}

// RecordFigures reads the records, activity, provider and spend figures for the workspace bound to tx.
func RecordFigures(ctx context.Context, tx pgx.Tx) (RecordsSummary, Activity, []ProviderRow, Spend, error) {
	var (
		rec RecordsSummary
		act = Activity{WindowDays: WindowDays, ByDay: []ActivityDay{}}
		spd = Spend{WindowDays: WindowDays, ByDay: []SpendDay{}}
		prv = []ProviderRow{}
	)
	const ws = `current_setting('app.workspace_id', true)`
	if err := tx.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM company_entities WHERE workspace_id=`+ws+`),
  (SELECT count(*) FROM person_entities WHERE workspace_id=`+ws+`),
  (SELECT count(*) FROM person_entities WHERE workspace_id=`+ws+` AND lower(trim(coalesce(accepted_fields->>'email',''))) NOT IN ('','n/a')),
  (SELECT count(*) FROM person_entities WHERE workspace_id=`+ws+` AND lower(trim(coalesce(accepted_fields->>'email',''))) NOT IN ('','n/a')
     AND lower(coalesce(nullif(accepted_fields->>'email_verify_status',''),accepted_fields->'email_verify'->>'status',accepted_fields->>'email_verify','')) IN ('valid','verified','deliverable')),
  (SELECT count(*) FROM company_entities WHERE workspace_id=`+ws+` AND nullif(trim(coalesce(accepted_fields->>'domain',primary_domain,'')),'') IS NULL)`).
		Scan(&rec.Companies, &rec.People, &rec.PeopleWithEmail, &rec.VerifiedEmails, &rec.CompaniesNeedingDomain); err != nil {
		return rec, act, prv, spd, fmt.Errorf("analytics: records: %w", err)
	}
	rec.EmailPct, rec.VerifiedPct = Pct(rec.PeopleWithEmail, rec.People), Pct(rec.VerifiedEmails, rec.People)

	if err := tx.QueryRow(ctx, `SELECT count(*),
  count(*) FILTER (WHERE last_run_at >= now() - make_interval(days => $1)),
  count(*) FILTER (WHERE status='running'),
  count(*) FILTER (WHERE status='failed')
FROM workbooks WHERE workspace_id=`+ws, WindowDays).
		Scan(&act.Workbooks, &act.WorkbooksRun, &act.RunningWorkbooks, &act.FailedWorkbooks); err != nil {
		return rec, act, prv, spd, fmt.Errorf("analytics: workbooks: %w", err)
	}

	// One row per day in the window, including days with no writes, so the chart axis is complete.
	rows, err := tx.Query(ctx, `SELECT to_char(d.day,'YYYY-MM-DD'), coalesce(c.filled,0), coalesce(c.no_data,0)
FROM (SELECT current_date - ($1::int - 1) + n AS day FROM generate_series(0, $1::int - 1) n) AS d
LEFT JOIN (
  SELECT updated_at::date AS day,
         count(*) FILTER (WHERE status='complete') AS filled,
         count(*) FILTER (WHERE status='error') AS no_data
  FROM workbook_enrichments
  WHERE workspace_id=`+ws+` AND updated_at >= current_date - ($1::int - 1)
  GROUP BY 1) c ON c.day = d.day
ORDER BY d.day`, WindowDays)
	if err != nil {
		return rec, act, prv, spd, fmt.Errorf("analytics: activity: %w", err)
	}
	for rows.Next() {
		var d ActivityDay
		if err := rows.Scan(&d.Date, &d.Filled, &d.NoData); err != nil {
			rows.Close()
			return rec, act, prv, spd, err
		}
		act.CellsFilled += d.Filled
		act.CellsNoData += d.NoData
		act.ByDay = append(act.ByDay, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rec, act, prv, spd, err
	}
	act.FillRatePct = Pct(act.CellsFilled, act.CellsFilled+act.CellsNoData)

	// Spend: settled attempts only; reserved, released and uncertain amounts were never charged.
	var windowMicro, totalMicro int64
	if err := tx.QueryRow(ctx, `SELECT
  coalesce(sum(settled_microusd) FILTER (WHERE created_at >= extract(epoch FROM current_date - ($1::int - 1))),0),
  coalesce(sum(settled_microusd),0)
FROM workbook_spend_attempts WHERE workspace_id=`+ws+` AND status='settled'`, WindowDays).Scan(&windowMicro, &totalMicro); err != nil {
		return rec, act, prv, spd, fmt.Errorf("analytics: spend: %w", err)
	}
	spd.WindowUSD, spd.TotalUSD = float64(windowMicro)/1e6, float64(totalMicro)/1e6
	srows, err := tx.Query(ctx, `SELECT to_char(to_timestamp(created_at) AT TIME ZONE 'UTC','YYYY-MM-DD'), sum(settled_microusd)
FROM workbook_spend_attempts
WHERE workspace_id=`+ws+` AND status='settled' AND created_at >= extract(epoch FROM current_date - ($1::int - 1))
GROUP BY 1 ORDER BY 1`, WindowDays)
	if err != nil {
		return rec, act, prv, spd, fmt.Errorf("analytics: spend by day: %w", err)
	}
	for srows.Next() {
		var d SpendDay
		var micro int64
		if err := srows.Scan(&d.Date, &micro); err != nil {
			srows.Close()
			return rec, act, prv, spd, err
		}
		d.USD = float64(micro) / 1e6
		spd.ByDay = append(spd.ByDay, d)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return rec, act, prv, spd, err
	}

	// Providers: filled cells (workbook_enrichments) joined with settled spend (workbook_spend_attempts).
	prows, err := tx.Query(ctx, `WITH filled AS (
  SELECT provider, count(*) AS n FROM workbook_enrichments
  WHERE workspace_id=`+ws+` AND status='complete' AND coalesce(provider,'')<>'' AND updated_at >= current_date - ($1::int - 1)
  GROUP BY 1),
paid AS (
  SELECT provider, count(*) AS attempts, coalesce(sum(settled_microusd),0) AS micro FROM workbook_spend_attempts
  WHERE workspace_id=`+ws+` AND status='settled' AND created_at >= extract(epoch FROM current_date - ($1::int - 1))
  GROUP BY 1)
SELECT coalesce(f.provider,p.provider), coalesce(f.n,0), coalesce(p.attempts,0), coalesce(p.micro,0)
FROM filled f FULL JOIN paid p ON p.provider = f.provider
ORDER BY coalesce(f.n,0) DESC, coalesce(p.micro,0) DESC, 1
LIMIT 25`, WindowDays)
	if err != nil {
		return rec, act, prv, spd, fmt.Errorf("analytics: providers: %w", err)
	}
	for prows.Next() {
		var r ProviderRow
		var micro int64
		if err := prows.Scan(&r.Provider, &r.CellsFilled, &r.PaidAttempts, &micro); err != nil {
			prows.Close()
			return rec, act, prv, spd, err
		}
		r.SpendUSD = float64(micro) / 1e6
		r.SharePct = Pct(r.CellsFilled, act.CellsFilled)
		prv = append(prv, r)
	}
	prows.Close()
	return rec, act, prv, spd, prows.Err()
}
