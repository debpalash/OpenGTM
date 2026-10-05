// Package enrich is the Go executor for the run_workbook_connector job type: a
// workbook enrichment run whose cells are provider waterfalls made only of
// manifest v1 connectors, over workbook rows that are not linked to a lead.
//
// It is a behavioural port of that slice of
// apps/api/services/workbook/enrichment.py (handle_run_workbook,
// enrich_cell, _run_one_row and the retry passes), the spend ledger
// (spend_service.py, provider_accounting.py) and the planner's chain filtering
// and telemetry. The Python code stays the source of truth; the cross-language
// harness (tests/test_enrichment_go_parity_pg.py) runs both on identical
// databases and provider responses and compares the stored state.
//
// The job type is Python-owned until an operator routes it:
//
//	opengtm routes set run_workbook_connector go      # cut over
//	opengtm routes set run_workbook_connector python  # roll back
//
// See docs/plans/m2-enrichment-slice.md for what is and is not in scope.
package enrich

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
	"github.com/debpalash/OpenGTM/apps/server/internal/pluginrun"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// JobType is the queue job type this package executes.
const JobType = "run_workbook_connector"

// Connectors is the set of installed manifest v1 connectors, by provider name
// (satisfied by *pluginrun.Catalog).
type Connectors interface {
	Get(name string) (*pluginrun.Entry, bool)
}

// Worker executes run_workbook_connector jobs.
type Worker struct {
	pool      *pgxpool.Pool
	catalog   Connectors
	client    *egress.Client
	log       *slog.Logger
	domainRPS float64
}

// NewWorker builds the job handler. catalog supplies the v1 connectors, client
// the guarded egress (shared by every job of the process).
func NewWorker(pool *pgxpool.Pool, catalog Connectors, client *egress.Client, log *slog.Logger, domainRPS float64) *Worker {
	if log == nil {
		log = slog.Default()
	}
	if domainRPS <= 0 {
		domainRPS = DefaultDomainRPS
	}
	return &Worker{pool: pool, catalog: catalog, client: client, log: log, domainRPS: domainRPS}
}

// Register adds the handler to a queue registry. There is no failure
// reconciler: the Python job type has none, and the run's own finalization
// keeps the workbook out of `running`.
func (w *Worker) Register(r *queue.Registry) { r.Register(JobType, w.Handle) }

// plugin returns the loaded manifest v1 connector called name, or nil.
func (w *Worker) plugin(name string) *manifest.Plugin {
	e, ok := w.catalog.Get(name)
	if !ok || e.Source != "connector" || e.Plugin.Provider == nil || e.Plugin.V1 == nil {
		return nil
	}
	return e.Plugin
}

// Handle runs one attempt of a workbook run.
func (w *Worker) Handle(ctx context.Context, job queue.Job) error {
	pl, err := parsePayload(job.Payload)
	if err != nil {
		return err
	}
	if job.WorkspaceID != "" && job.WorkspaceID != pl.WorkspaceID {
		return fmt.Errorf("payload workspace %q does not match job workspace %q", pl.WorkspaceID, job.WorkspaceID)
	}
	log := w.log.With("job_id", job.ID, "workbook_id", pl.WorkbookID)
	log.Info("run_workbook_connector started", "attempt", job.RetryCount+1)

	r := &run{w: w, job: job, pl: pl, ws: pl.WorkspaceID, wbID: pl.WorkbookID}
	r.led = &ledger{pool: w.pool, job: job}
	r.caller = &providerCaller{
		client: w.client, sem: make(chan struct{}, pl.ProviderWorkers),
		timeout: secondsDuration(pl.ProviderTimeout), rps: w.domainRPS,
	}
	r.stop = &stopProbe{r: r}

	res, err := r.execute(ctx)
	if err != nil {
		return err
	}
	log.Info("run_workbook_connector done", "completed", res.completed, "errors", res.errors, "total", res.total,
		"rows", res.rows, "stopped", res.stopped)
	if res.receipt {
		// A diagnostic receipt must not turn executed external calls into a retry.
		if perr := r.persistReceipt(ctx, res); perr != nil {
			log.Warn("could not persist the run receipt", "err", perr)
		}
	}
	return nil
}

var refRE = regexp.MustCompile(`\{([^}]+)\}`)

// stringsHaveRef reports whether any string leaf of v contains a {reference}.
func stringsHaveRef(v any) bool {
	switch t := v.(type) {
	case string:
		return refRE.MatchString(t)
	case *pycompat.Map:
		for _, e := range t.Entries() {
			if stringsHaveRef(e.Value) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if stringsHaveRef(e) {
				return true
			}
		}
	}
	return false
}
