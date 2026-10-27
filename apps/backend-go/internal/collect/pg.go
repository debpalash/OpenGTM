package collect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectintent"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Postgres adapters: the collection ledger (PgJobLedger), the workspace lead store
// (PgLeadStore) and the global source_stats ledger. Every call is its own transaction, as each
// PgJobLedger / PgLeadStore method was.

func nowISO() string { return collectledger.Isoformat(time.Now().UTC()) }

// PgLedger is the Ledger of one workspace.
type PgLedger struct {
	Pool *pgxpool.Pool
	WS   string
}

func (p PgLedger) tx(ctx context.Context, fn func(q *gen.Queries, tx pgx.Tx) error) error {
	return tenancy.InWorkspace(ctx, p.Pool, p.WS, func(tx pgx.Tx) error { return fn(gen.New(tx), tx) })
}

func (p PgLedger) MarkRunning(ctx context.Context, id string) error {
	return p.tx(ctx, func(q *gen.Queries, _ pgx.Tx) error {
		n, err := q.CollectMarkRunning(ctx, gen.CollectMarkRunningParams{ID: id, StartedAt: nowISO()})
		if err == nil && n == 0 {
			return ErrNotPending
		}
		return err
	})
}

func (p PgLedger) MarkFailed(ctx context.Context, id, msg string) error {
	return p.tx(ctx, func(_ *gen.Queries, tx pgx.Tx) error {
		return collectledger.MarkEnqueueFailed(ctx, tx, id, msg, time.Now().UTC())
	})
}

func (p PgLedger) CreateStage(ctx context.Context, jobID, stage string) (int64, error) {
	var id int64
	err := p.tx(ctx, func(q *gen.Queries, _ pgx.Tx) error {
		var err error
		id, err = q.CollectCreateStage(ctx, gen.CollectCreateStageParams{JobID: jobID, Stage: stage, StartedAt: nowISO()})
		return err
	})
	return id, err
}

func (p PgLedger) CompleteStage(ctx context.Context, id int64, r StageResult) error {
	return p.tx(ctx, func(q *gen.Queries, _ pgx.Tx) error {
		_, err := q.CollectCompleteStage(ctx, gen.CollectCompleteStageParams{ID: id, Status: r.Status,
			InputCount: int32(r.InputCount), OutputCount: int32(r.OutputCount), RejectedCount: int32(r.RejectedCount),
			Details: r.Details, CompletedAt: nowISO()})
		return err
	})
}

func (p PgLedger) CompleteJob(ctx context.Context, id string, n int) error {
	return p.tx(ctx, func(q *gen.Queries, _ pgx.Tx) error {
		_, err := q.CollectCompleteJob(ctx, gen.CollectCompleteJobParams{ID: id, CompletedAt: nowISO(), LeadsFound: int32(n)})
		return err
	})
}

func (p PgLedger) FailJob(ctx context.Context, id, msg string) error {
	return p.tx(ctx, func(q *gen.Queries, _ pgx.Tx) error {
		_, err := q.CollectFailJob(ctx, gen.CollectFailJobParams{ID: id, Error: msg, CompletedAt: nowISO()})
		return err
	})
}

// Status is get_status ("" for a missing job).
func (p PgLedger) Status(ctx context.Context, id string) (string, error) {
	var st string
	err := p.tx(ctx, func(q *gen.Queries, _ pgx.Tx) error {
		row, err := q.CJGet(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		st = row.Status
		return err
	})
	return st, err
}

// Detail is get_job_detail's (status, error, intent); found false for a missing job.
func (p PgLedger) Detail(ctx context.Context, id string) (status, errMsg, intent string, found bool, err error) {
	err = p.tx(ctx, func(q *gen.Queries, _ pgx.Tx) error {
		row, e := q.CJGet(ctx, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		status, errMsg, intent, found = row.Status, row.Error, row.Intent, true
		return nil
	})
	return
}

// PgStore is the Store of one workspace.
type PgStore struct {
	Pool *pgxpool.Pool
	WS   string
}

func (s PgStore) ExistingCompanies(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	err := tenancy.InWorkspace(ctx, s.Pool, s.WS, func(tx pgx.Tx) error {
		names, err := gen.New(tx).CollectWorkspaceCompanies(ctx)
		for _, n := range names {
			out[pyStrip(pyLower(n))] = true
		}
		return err
	})
	return out, err
}

// leadColumns are the Lead dataclass fields that are leads columns, in dataclass order (id and
// workspace_id are handled separately).
var leadColumns = []string{"company", "website", "email", "email_confidence", "email_provider", "phone", "phone_provider",
	"contact_person", "contact_title", "city", "state", "address", "specialization", "company_size",
	"employee_count_exact", "description", "revenue_range", "founded_year", "industry_tags", "technologies",
	"technographics", "funding_stage", "company_size_basis", "linkedin_url", "twitter_url", "facebook_url",
	"secondary_emails", "secondary_phones", "decision_makers", "glassdoor_rating", "hiring_signals",
	"enrichment_attempts", "enrichment_waterfall", "field_provenance", "source", "source_url", "collection_job_id",
	"score", "score_tier", "status", "yupcha_value_prop", "company_need", "notes", "created_at", "updated_at",
	"last_enriched_at"}

var upsertSQL = func() string {
	ph := []string{"current_setting('app.workspace_id', true)"}
	var sets []string
	for i, c := range leadColumns {
		ph = append(ph, fmt.Sprintf("$%d", i+1))
		switch c {
		case "company", "city", "created_at", "status", "notes", "score", "score_tier":
			// The conflict key, and what the user owns: a re-found company never resets them.
		case "updated_at":
			sets = append(sets, c+" = EXCLUDED."+c)
		case "employee_count_exact", "enrichment_attempts":
			sets = append(sets, c+" = COALESCE(NULLIF(leads."+c+", 0), EXCLUDED."+c+")")
		default:
			// Collection only fills what is still empty; known values (and the owning job) stay.
			sets = append(sets, c+" = COALESCE(NULLIF(leads."+c+", ''), EXCLUDED."+c+")")
		}
	}
	return "INSERT INTO leads (workspace_id, " + strings.Join(leadColumns, ", ") + ") VALUES (" + strings.Join(ph, ", ") +
		") ON CONFLICT (workspace_id, company, city) DO UPDATE SET " + strings.Join(sets, ", ") + " RETURNING id"
}()

// Upsert is upsert_lead: insert, or update the (workspace, company, city) row in one statement
// (legacy read-then-write raced with a concurrent insert). Fix: legacy overwrote every column of
// a re-found company; here status, notes, score and score_tier (user-owned) are never touched and
// every other column is only filled when empty.
func (s PgStore) Upsert(ctx context.Context, l *Lead) error {
	l.UpdatedAt = isoformat(time.Now().UTC())
	m := l.Map()
	args := make([]any, len(leadColumns))
	for i, c := range leadColumns {
		switch v := m[c].(type) {
		case int:
			args[i] = int32(v)
		default:
			args[i] = v
		}
	}
	return tenancy.InWorkspace(ctx, s.Pool, s.WS, func(tx pgx.Tx) error {
		var id int32
		if err := tx.QueryRow(ctx, upsertSQL, args...).Scan(&id); err != nil {
			return fmt.Errorf("collect: upsert lead: %w", err)
		}
		l.ID = int64(id)
		return nil
	})
}

// RecordSourceRun is source_stats.record_run (global table, one atomic upsert per source).
func (s PgStore) RecordSourceRun(ctx context.Context, region string, runs []SourceRun) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := gen.New(tx)
		for _, r := range runs {
			withOutput := int32(0)
			if r.Emitted > 0 {
				withOutput = 1
			}
			if err := q.SourceStatsRecord(ctx, gen.SourceStatsRecordParams{Source: r.Source, Region: region,
				RunsWithOutput: withOutput, Emitted: int32(r.Emitted), SurvivedDedup: int32(r.SurvivedDedup),
				Validated: int32(r.Validated), BatchReliability: r.batchReliability()}); err != nil {
				return err
			}
		}
		return nil
	})
}

// batchReliability is SourceRunCounters.batch_reliability.
func (c SourceCounters) batchReliability() float64 {
	if c.Emitted <= 0 {
		return 0
	}
	return 0.5*1.0 + 0.25*(float64(c.SurvivedDedup)/float64(c.Emitted)) + 0.25*(float64(c.Validated)/float64(c.Emitted))
}

// Reliability is source_stats.reliability_map.
func (s PgStore) Reliability(ctx context.Context, region string) (map[string]float64, error) {
	rows, err := gen.New(s.Pool).SourceStatsReliability(ctx, region)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Source] = r.Reliability
	}
	return out, nil
}

// ── submit (JobRunner.submit) ───────────────────────────────────────────────

// ErrBlocked is submit's ValueError: decide_collection refused the query.
type ErrBlocked struct{ Reason string }

func (e *ErrBlocked) Error() string { return e.Reason }

// Submitter runs a collection inline (JobRunner.submit): create the ledger row, emit job_created,
// process. It is the source_workbook collector (internal/sourcewb Collector).
type Submitter struct {
	Pool   *pgxpool.Pool
	Runner *Runner
	// Emit returns the progress emitter of a workspace.
	Emit func(ws string) Emitter
	// NewID returns a uuid4 hex id (tests); nil = random.
	NewID func() string
}

// Submit is JobRunner.submit(query, workspace_id): ErrBlocked when the intent gate refuses, else
// the job id once the run ended (the job's own outcome is in its ledger row; a pipeline failure
// is also returned).
func (s Submitter) Submit(ctx context.Context, ws, query string) (string, error) {
	d := collectintent.Decide(query, nil)
	if !d.CanCollect() {
		return "", &ErrBlocked{d.Reason}
	}
	id := newJobID()
	if s.NewID != nil {
		id = s.NewID()
	}
	if err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		return collectledger.Create(ctx, tx, ws, id, query, d.Intent, "{}", time.Now().UTC())
	}); err != nil {
		return "", err
	}
	em := s.Emit(ws)
	if em != nil {
		em.Emit(ctx, "job_created", obj("job_id", id, "query", query, "workspace_id", ws))
	}
	err := s.Runner.Process(ctx, Job{ID: id, Query: query, Intent: d.Intent, WorkspaceID: ws,
		Ledger: PgLedger{s.Pool, ws}, Store: PgStore{s.Pool, ws}, Emit: em})
	if errors.Is(err, ErrCancelled) {
		err = nil
	}
	return id, err
}
