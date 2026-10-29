package workbook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// RunSummary is the dict run_workbook_enrichment returns to the automation
// re_enrich action. BudgetExceeded mirrors the key the Python action reads; the
// Python run never sets it, so it is false unless a future run reports it.
type RunSummary struct {
	Completed, Errors int
	BudgetExceeded    bool
	Error             string
}

// RunRows runs the given columns for the given rows of one workbook in the
// caller's process, with no queue lease (run_workbook_enrichment called
// directly: every lease fence is a no-op, nothing can cancel it but ctx or a
// workbook pause). It is the very same loop as the queued executor, one row at
// a time. A returned error is infrastructure or a refusal the queued executor
// would have handed back; the caller reports it as the action's failure.
func RunRows(ctx context.Context, deps Deps, ws, workbookID string, columnIDs []string, rowIDs []int32, providerTimeout time.Duration) (RunSummary, error) {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	if deps.Runner == nil {
		return RunSummary{}, errors.New("workbook: RunRows needs a provider runner")
	}
	secs := providerTimeout.Seconds()
	if !(secs > 0) {
		secs = defaultProviderTimeout
	}
	secs = min(secs, MaxProviderTimeout)
	ids := make(map[string]bool, len(columnIDs))
	for _, c := range columnIDs {
		ids[c] = true
	}
	p := &runPayload{WorkspaceID: ws, WorkbookID: workbookID, Concurrency: 1, RetryPasses: 1,
		ProviderTimeout: secs, ColumnIDs: ids, RowIDs: rowIDs, HasRowIDs: true,
		Manifests: map[string]manifest.Manifest{}, Costs: map[string]float64{}}
	interval := deps.StopCheckInterval
	if interval <= 0 {
		interval = defaultStopCheckInterval
	}
	rs := &runState{deps: deps, p: p, pl: planner{catalog: true}, log: log, runner: deps.Runner}
	// Spend identity of a run without a job: its own run, as an in-request cell has.
	id := time.Now().UnixNano()
	rs.cell = &cellIdentity{keyID: id, runID: fmt.Sprintf("rows:%d", id)}
	rs.stop = &stopProbe{interval: interval, check: func() bool { return rs.pausedOrCancelled(ctx) }}
	var started bool
	res, err := rs.runBody(ctx, &started)
	if err == nil && res.skipReceipt {
		return RunSummary{Error: "workbook_not_found"}, nil // {"error": "workbook_not_found"}
	}
	if err != nil {
		return RunSummary{Completed: res.Completed, Errors: res.Errors, Error: err.Error()}, err
	}
	return RunSummary{Completed: res.Completed, Errors: res.Errors}, nil
}
