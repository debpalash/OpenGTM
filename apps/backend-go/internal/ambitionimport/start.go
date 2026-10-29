package ambitionimport

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionbox"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourceingest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// PageFetcher is the AmbitionBox client's page call (*ambitionbox.Client).
type PageFetcher interface {
	FetchCompanyPage(ctx context.Context, p ambitionbox.PageParams) (sourceingest.Page, error)
}

// Deps are the collaborators of Start, Handle and Reconcile.
type Deps struct {
	Pool    *pgxpool.Pool
	Fetcher PageFetcher // build the client with the egress SSRF-guarded http client
	// Hook fires on_row_added for rows a page added, inside the page transaction.
	Hook automationhook.Hook
	// Progress carries the best-effort source_page event (nil = none).
	Progress progress.Publisher
	Log      *slog.Logger
	// Executor picks the executor of a new job per workspace; nil = "go", the only
	// executor since the plan 15 cutover. See ExecutorFor.
	Executor func(workspaceID string) string
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// ExecutorFor names the executor of a new ambitionbox_import job: always "go" (GO_EXECUTOR_AMBITIONBOX and
// the cohort flags are retired, plan 15 WP-CUTOVER). getenv is no longer consulted.
func ExecutorFor(func(string) string) func(string) string {
	return func(string) string { return "go" }
}

// StartParams are the inputs of start_ambitionbox_import.
type StartParams struct {
	WorkspaceID, Name string
	Industry, Rating  *string
	SortBy            string
	Limit             int
	WorkbookID        *string
	ReplaceExisting   bool
}

// StartResult is the start_ambitionbox_import response (key order is the contract).
type StartResult struct {
	WorkbookID     string `json:"workbook_id"`
	Name           string `json:"name"`
	RunID          string `json:"run_id"`
	JobID          int64  `json:"job_id"`
	Status         string `json:"status"`
	RequestedLimit int    `json:"requested_limit"`
	Source         string `json:"source"`
	URL            string `json:"url"`
}

var (
	// ErrNeedsReplace: an existing non-AmbitionBox workbook without replace_existing.
	ErrNeedsReplace = errors.New("existing non-AmbitionBox workbooks require replace_existing=true")
	// ErrWorkbookNotFound: unknown or foreign workbook.
	ErrWorkbookNotFound = errors.New("workbook not found in the active workspace")
)

// AlreadyRunningError is AmbitionBoxImportAlreadyRunning.
type AlreadyRunningError struct{ RunID string }

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("AmbitionBox import %s is already active", e.RunID)
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func optStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func marshal(o *wbcrud.Obj) (string, error) {
	b, err := o.MarshalJSON()
	return string(b), err
}

func columnsText() string { return columnsJSON }

// Start is start_ambitionbox_import: the workbook, the pending connector run
// and the job are created in one transaction.
func Start(ctx context.Context, d Deps, p StartParams) (StartResult, error) {
	limit := p.Limit
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	maxPages := (limit+19)/20 + 5
	if maxPages > 25 {
		maxPages = 25
	}
	if maxPages < 1 {
		maxPages = 1
	}
	executor := "go"
	if d.Executor != nil {
		executor = d.Executor(p.WorkspaceID)
	}
	if executor != "go" {
		return StartResult{}, fmt.Errorf("ambitionimport: executor %q must be go (the only executor since the plan 15 cutover)", executor)
	}
	name := strings.TrimSpace(p.Name)
	workbookID := ""
	if p.WorkbookID != nil {
		workbookID = *p.WorkbookID
	}

	var res StartResult
	err := tenancy.InWorkspace(ctx, d.Pool, p.WorkspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		wbID, wbName := workbookID, ""
		if workbookID != "" {
			wb, err := q.AIWorkbookForStart(ctx, workbookID)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && wb.WorkspaceID != p.WorkspaceID) {
				return ErrWorkbookNotFound
			}
			if err != nil {
				return err
			}
			if active, err := q.AIActiveRun(ctx, workbookID); err == nil {
				return &AlreadyRunningError{RunID: active}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if !p.ReplaceExisting && wb.SourceType.String != "ambitionbox" {
				return ErrNeedsReplace
			}
			wbName = wb.Name
			if name != "" {
				wbName = name
			}
			if err := q.AIRepurposeWorkbook(ctx, gen.AIRepurposeWorkbookParams{
				Name: name, ColumnsConfig: columnsText(), WorkbookID: workbookID, WorkspaceID: p.WorkspaceID}); err != nil {
				return err
			}
		} else {
			wbID = uuid4()
			wbName = name
			if wbName == "" {
				wbName = "AmbitionBox Companies"
			}
			desc := "Durable AmbitionBox company import"
			if p.Industry != nil && *p.Industry != "" {
				desc += " for " + *p.Industry
			}
			if _, err := q.AIInsertWorkbook(ctx, gen.AIInsertWorkbookParams{ID: wbID, Name: wbName, Description: desc,
				WorkspaceID: p.WorkspaceID, ColumnsConfig: columnsText()}); err != nil {
				return err
			}
		}

		query := wbcrud.NewObj()
		query.Set("industry", optStr(p.Industry))
		query.Set("rating", optStr(p.Rating))
		query.Set("sort_by", p.SortBy)
		query.Set("max_pages", maxPages)
		query.Set("replace_existing", p.ReplaceExisting)
		cursor := wbcrud.NewObj()
		cursor.Set("next_page", 1)
		cursor.Set("reset_done", false)
		cursor.Set("seen_record_ids", []any{})
		qs, err := marshal(query)
		if err != nil {
			return err
		}
		cs, err := marshal(cursor)
		if err != nil {
			return err
		}
		runID := uuid4()
		if err := q.AIInsertRun(ctx, gen.AIInsertRunParams{ID: runID, WorkspaceID: p.WorkspaceID, WorkbookID: wbID,
			Query: qs, Cursor: cs, RequestedCount: int32(limit)}); err != nil {
			return err
		}
		sc := wbcrud.NewObj()
		sc.Set("provider", "ambitionbox")
		for _, k := range query.Keys() {
			v, _ := query.Get(k)
			sc.Set(k, v)
		}
		sc.Set("requested_limit", limit)
		sc.Set("active_run_id", runID)
		scs, err := marshal(sc)
		if err != nil {
			return err
		}
		if err := q.AISetSourceConfig(ctx, gen.AISetSourceConfigParams{SourceConfig: scs, WorkbookID: wbID, WorkspaceID: p.WorkspaceID}); err != nil {
			return err
		}
		pl := wbcrud.NewObj()
		pl.Set("workspace_id", p.WorkspaceID)
		pl.Set("workbook_id", wbID)
		pl.Set("run_id", runID)
		pls, err := marshal(pl)
		if err != nil {
			return err
		}
		jobID, err := q.AIInsertJob(ctx, gen.AIInsertJobParams{Executor: executor, Payload: pls, WorkspaceID: p.WorkspaceID})
		if err != nil {
			return fmt.Errorf("ambitionimport: enqueue: %w", err)
		}
		res = StartResult{WorkbookID: wbID, Name: wbName, RunID: runID, JobID: int64(jobID), Status: "pending",
			RequestedLimit: limit, Source: "ambitionbox", URL: "/workbooks/" + wbID}
		return nil
	})
	if err == nil {
		return res, nil
	}
	var pg *pgconn.PgError
	if workbookID != "" && errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "uq_connector_runs_active" {
		// The partial unique index is the final arbiter (defence in depth).
		var active string
		rerr := tenancy.InWorkspace(ctx, d.Pool, p.WorkspaceID, func(tx pgx.Tx) error {
			var e error
			active, e = gen.New(tx).AIActiveRun(ctx, workbookID)
			return e
		})
		if rerr == nil {
			return StartResult{}, &AlreadyRunningError{RunID: active}
		}
	}
	return StartResult{}, err
}
