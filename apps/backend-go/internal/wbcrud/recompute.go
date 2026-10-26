package wbcrud

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// PerfValues are the enrichment performance settings stamped into a run job
// (routers/settings.get_enrichment_settings).
type PerfValues struct {
	RowConcurrency, ProviderWorkers, ProviderTimeout, MaxProviders, RetryPasses int
}

// DefaultPerf mirrors _ENRICHMENT_DEFAULTS.
var DefaultPerf = PerfValues{RowConcurrency: 12, ProviderWorkers: 8, ProviderTimeout: 10, MaxProviders: 8, RetryPasses: 1}

// Recomputer starts the reactive recompute that follows a row edit: the
// run_workbook call of update_row_data / bulk_update_row_data. Python enqueues
// ONE durable run_workbook job for its DB-polling worker; so does this port,
// in the caller's transaction, with executor 'python'.
//
// Not ported (plan 05, retained /run): the platform-credit debit when billing
// is enabled (BillingEnabled makes Start report "unavailable" instead of
// running unmetered) and Python's Go-executor eligibility (a cohort workspace
// would be routed to the Go executor; this path always uses 'python').
type Recomputer struct {
	Perf           func(context.Context) (PerfValues, error) // nil: defaults
	BillingEnabled bool
}

// Unavailable is the recompute result reported when a run cannot be started here.
func Unavailable() *Obj {
	o := NewObj()
	o.Set("status", "unavailable")
	o.Set("detail", "Recompute with platform billing enabled is not supported by this backend; run the affected columns manually")
	return o
}

// Start enqueues the run for columnIDs over rowIDs (rowColumns: exact cells per
// row, nil for every selected column) and returns the RunWorkbookResponse
// fields {status,total_jobs,message,job_id,run_id}. A *GuardError carries the
// HTTPException Python would raise (422 cycle, 409 rows changed).
func (r *Recomputer) Start(ctx context.Context, tx pgx.Tx, ws string, w Workbook, columnIDs []any, rowIDs []int64, rowColumns *Obj) (*Obj, *GuardError, error) {
	if r.BillingEnabled {
		return Unavailable(), nil, nil
	}
	var cols []Column
	for _, c := range w.Columns {
		typ, _ := c.Get("type").(string)
		if !runnableTypes[typ] {
			continue
		}
		for _, id := range columnIDs {
			if sameValue(c.Get("id"), id) {
				cols = append(cols, c)
				break
			}
		}
	}
	sort.SliceStable(cols, func(i, j int) bool {
		return cols[i].Get("type") != "output" && cols[j].Get("type") == "output"
	})
	if len(cols) == 0 {
		return runResponse("skipped", 0, "No enrichment columns to run", nil, ""), nil, nil
	}
	selected := map[string]struct{}{}
	if rowColumns != nil {
		for _, k := range rowColumns.Keys() {
			v, _ := rowColumns.Get(k)
			for _, id := range asList(v) {
				selected[pyStr(id)] = struct{}{}
			}
		}
	} else {
		for _, c := range cols {
			selected[pyStr(c.Get("id"))] = struct{}{}
		}
	}
	var cyclic []string
	for id := range CycleBlockedColumns(w.Columns) {
		if _, ok := selected[id]; ok {
			cyclic = append(cyclic, id)
		}
	}
	if len(cyclic) > 0 {
		sort.Strings(cyclic)
		return nil, &GuardError{Status: 422, Detail: "Selected columns depend on a circular reference: " + strings.Join(cyclic, ", ") +
			". Edit their column references to break the cycle before running."}, nil
	}

	q := gen.New(tx)
	found, err := q.ExistingWorkbookRowIDs(ctx, gen.ExistingWorkbookRowIDsParams{WorkbookID: w.ID, RowIds: rowIDs})
	if err != nil {
		return nil, nil, err
	}
	resolved := make([]int64, len(found))
	for i, id := range found {
		resolved[i] = int64(id)
	}
	if rowColumns != nil {
		if !sameIDSet(resolved, rowIDs) {
			return nil, &GuardError{Status: 409, Detail: "Selected workbook rows changed. Refresh before running."}, nil
		}
		kept := resolved[:0:0]
		for _, id := range resolved {
			if v, _ := rowColumns.Get(strconv.FormatInt(id, 10)); truthy(v) {
				kept = append(kept, id)
			}
		}
		resolved = kept
	}
	if len(resolved) == 0 {
		return runResponse("skipped", 0, "No rows to process", nil, ""), nil, nil
	}

	runID := strings.ReplaceAll(NewID(), "-", "")
	if err := q.StartWorkbookRun(ctx, gen.StartWorkbookRunParams{TotalRows: int32(len(resolved)), WorkbookID: w.ID, WorkspaceID: ws}); err != nil {
		return nil, nil, err
	}
	totalJobs := len(resolved) * len(cols)
	if rowColumns != nil {
		totalJobs = 0
		for _, id := range resolved {
			v, _ := rowColumns.Get(strconv.FormatInt(id, 10))
			uniq := map[string]struct{}{}
			for _, c := range asList(v) {
				uniq[pyStr(c)] = struct{}{}
			}
			totalJobs += len(uniq)
		}
	}
	if err := q.CancelActiveRunJobs(ctx, w.ID); err != nil {
		return nil, nil, err
	}
	perf := DefaultPerf
	if r.Perf != nil {
		if p, err := r.Perf(ctx); err == nil {
			perf = p
		}
	}
	colIDs := make([]any, len(cols))
	for i, c := range cols {
		colIDs[i] = c.Get("id")
	}
	rowList := make([]any, len(resolved))
	for i, id := range resolved {
		rowList[i] = id
	}
	payload := NewObj()
	payload.Set("workbook_id", w.ID)
	payload.Set("workspace_id", ws)
	payload.Set("run_id", runID)
	payload.Set("column_ids", colIDs)
	if rowColumns != nil {
		rc := NewObj()
		for _, id := range resolved {
			k := strconv.FormatInt(id, 10)
			v, _ := rowColumns.Get(k)
			rc.Set(k, v)
		}
		payload.Set("row_columns", rc)
	} else {
		payload.Set("row_columns", nil)
	}
	payload.Set("row_ids", rowList)
	payload.Set("lead_ids", nil)
	payload.Set("concurrency", perf.RowConcurrency)
	payload.Set("max_providers", perf.MaxProviders)
	payload.Set("retry_passes", perf.RetryPasses)
	payload.Set("provider_workers", perf.ProviderWorkers)
	payload.Set("provider_timeout", perf.ProviderTimeout)
	payload.Set("fill_missing", false)
	payload.Set("force", true)
	jobID, err := queue.Enqueue(ctx, tx, "run_workbook", ws, payload, "")
	if err != nil {
		return nil, nil, err
	}
	id := jobID
	return runResponse("started", totalJobs, fmt.Sprintf("Enqueued run: %d rows × %d columns", len(resolved), len(cols)), &id, runID), nil, nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func sameIDSet(a, b []int64) bool {
	x, y := map[int64]struct{}{}, map[int64]struct{}{}
	for _, v := range a {
		x[v] = struct{}{}
	}
	for _, v := range b {
		y[v] = struct{}{}
	}
	if len(x) != len(y) {
		return false
	}
	for v := range x {
		if _, ok := y[v]; !ok {
			return false
		}
	}
	return true
}

// runResponse is RunWorkbookResponse.model_dump().
func runResponse(status string, total int, message string, jobID *int64, runID string) *Obj {
	o := NewObj()
	o.Set("status", status)
	o.Set("total_jobs", int64(total))
	o.Set("message", message)
	if jobID != nil {
		o.Set("job_id", *jobID)
	} else {
		o.Set("job_id", nil)
	}
	if runID != "" {
		o.Set("run_id", runID)
	} else {
		o.Set("run_id", nil)
	}
	return o
}
