// Package workbook is the Go run_workbook executor for declarative-waterfall
// columns. It ports _run_workbook_enrichment_impl and the waterfall branch of
// enrich_cell (legacy/python/apps/api/services/workbook/enrichment.py).
package workbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const finalizeTimeout = 10 * time.Second

type workItem struct {
	lead map[string]any
	cols []column
}

func (w workItem) rowID() int64 { return intOf(w.lead["__row_id"]) }

// Run executes one claimed run_workbook job. The returned error fails the job
// (the queue retries it); ErrLeaseLost and context cancellation mean this
// executor must stop writing and leave the claim to its new owner.
func Run(ctx context.Context, deps Deps, c *queue.Claim) (Result, error) {
	var started bool
	res, err := run(ctx, deps, c, &started)
	if err != nil && !started && ctx.Err() == nil && !errors.Is(err, ErrLeaseLost) {
		if reason, ok := refusalReason(err); ok {
			// Deterministic refusal: Python runs the job normally. No retry is
			// spent and the workbook is not marked failed (nothing failed).
			handed, herr := queue.HandBack(ctx, deps.Pool, c, reason)
			switch {
			case herr == nil && handed:
				if deps.Log != nil {
					deps.Log.Warn("handed job back to python", "reason", reason)
				}
				return res, fmt.Errorf("%w: %s", ErrHandedBack, reason)
			case herr == nil:
				return res, ErrLeaseLost
			default:
				if deps.Log != nil {
					deps.Log.Error("hand back failed; failing the job instead", "err", herr)
				}
			}
		}
		failWorkbook(ctx, deps, c)
	}
	return res, err
}

// failWorkbook covers refusals and load errors that happen before the run loop
// (whose finalizer owns the status afterwards). /run already set the workbook
// to `running`, and nothing else would clear it if the job then fails
// terminally. Fenced, so only the live lease owner writes; a pause is kept.
// Cancellation and shutdown deliberately never reach here.
func failWorkbook(ctx context.Context, deps Deps, c *queue.Claim) {
	var p struct {
		WorkbookID string `json:"workbook_id"`
	}
	if json.Unmarshal(c.Payload, &p) != nil || p.WorkbookID == "" || c.WorkspaceID == "" {
		return
	}
	err := tenancy.InWorkspace(ctx, deps.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		if err := fence(ctx, tx, c, p.WorkbookID); err != nil {
			return err
		}
		return gen.New(tx).FailWorkbook(ctx, gen.FailWorkbookParams{WorkbookID: p.WorkbookID, WorkspaceID: c.WorkspaceID})
	})
	if err != nil && !errors.Is(err, ErrLeaseLost) && deps.Log != nil {
		deps.Log.Warn("could not mark workbook failed", "err", err)
	}
	if err == nil && deps.Progress != nil {
		deps.Progress.Publish(ctx, p.WorkbookID, map[string]any{"type": "workbook_status", "status": "failed",
			"completed": 0, "errors": 0, "total": 0})
	}
}

func run(ctx context.Context, deps Deps, c *queue.Claim, started *bool) (Result, error) {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	p, err := parsePayload(c.Payload)
	if err != nil {
		return Result{}, err
	}
	if p.WorkspaceID != c.WorkspaceID {
		return Result{}, errors.New("workbook: payload workspace does not match the claimed job")
	}
	if err := verifyManifests(ctx, deps.Manifests, p); err != nil {
		return Result{}, err
	}
	interval := deps.StopCheckInterval
	if interval <= 0 {
		interval = defaultStopCheckInterval
	}
	rs := &runState{deps: deps, claim: c, p: p, pl: planner{costs: p.Costs, catalog: deps.Runner != nil}, log: log}
	rs.runner, rs.legacy = deps.Runner, deps.Runner == nil
	if rs.legacy {
		rs.runner = manifestRunner{rs}
	}
	rs.stop = &stopProbe{interval: interval, check: func() bool { return rs.pausedOrCancelled(ctx) }}

	// ── Load config + rows ──
	wb, rows, err := rs.load(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Warn("workbook not found")
		return Result{skipReceipt: true}, nil
	}
	if err != nil {
		return Result{}, err
	}
	columns, err := parseColumns(wb.ColumnsConfig)
	if err != nil {
		return Result{}, err
	}
	rs.allCols = make([]colgraph.Col, len(columns))
	for i, col := range columns {
		rs.allCols[i] = col.g
	}
	rs.cols = columns
	rs.cycleBlocked = colgraph.CycleBlocked(rs.allCols)
	var enrichCols []column
	for _, col := range columns {
		if enrichmentColTypes[col.Type] && (p.ColumnIDs == nil || p.ColumnIDs[col.ID]) {
			if err := validateGoColumn(col); err != nil {
				return Result{}, err
			}
			if err := rs.checkPyColumn(col); err != nil {
				return Result{}, err
			}
			if rs.legacy && (col.Type == "enrichment" || col.Type == "waterfall") {
				if err := checkProviders(col, p); err != nil {
					return Result{}, err
				}
			}
			enrichCols = append(enrichCols, col)
		}
	}
	leads := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		var lid *int64
		if r.LeadID.Valid {
			v := int64(r.LeadID.Int32)
			lid = &v
		}
		d, err := rowExecutionData(int64(r.ID), lid, r.Data, r.Enrichments, columns)
		if err != nil {
			return Result{}, err
		}
		leads = append(leads, d)
	}

	if len(enrichCols) == 0 || len(leads) == 0 {
		err := tenancy.InWorkspace(ctx, deps.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
			if err := fence(ctx, tx, c, p.WorkbookID); err != nil {
				return err
			}
			return gen.New(tx).MarkWorkbookComplete(ctx, gen.MarkWorkbookCompleteParams{WorkbookID: p.WorkbookID, WorkspaceID: c.WorkspaceID})
		})
		return Result{Rows: len(leads)}, err
	}

	// Only the active queue lease may assert running state.
	if err := tenancy.InWorkspace(ctx, deps.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		if err := fence(ctx, tx, c, p.WorkbookID); err != nil {
			return err
		}
		return gen.New(tx).MarkWorkbookRunning(ctx, gen.MarkWorkbookRunningParams{WorkbookID: p.WorkbookID, WorkspaceID: c.WorkspaceID})
	}); err != nil {
		return Result{}, err
	}

	*started = true
	res, runErr := rs.execute(ctx, enrichCols, leads)
	res.Rows = len(leads)
	rs.finalize(ctx, &res, runErr, len(leads))
	return res, runErr
}

func (rs *runState) load(ctx context.Context) (gen.GetWorkbookForRunRow, []gen.ListWorkbookRowsRow, error) {
	var wb gen.GetWorkbookForRunRow
	var rows []gen.ListWorkbookRowsRow
	p := rs.p
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, p.WorkspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		var err error
		if wb, err = q.GetWorkbookForRun(ctx, gen.GetWorkbookForRunParams{WorkbookID: p.WorkbookID, WorkspaceID: p.WorkspaceID}); err != nil {
			return err
		}
		n, err := q.CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: p.WorkbookID, WorkspaceID: p.WorkspaceID})
		if err != nil {
			return err
		}
		if n == 0 {
			// _load_workbook_leads: never widen a selected run, and the v1
			// legacy leads-store read is not available to this executor.
			if !p.HasRowIDs && !(p.HasLeadIDs && len(p.LeadIDs) == 0) && usesLegacyLeads(wb) {
				return errors.New("workbook: legacy lead-store rows are not supported by the go executor")
			}
			return nil
		}
		rows, err = q.ListWorkbookRows(ctx, gen.ListWorkbookRowsParams{WorkbookID: p.WorkbookID, WorkspaceID: p.WorkspaceID,
			ByRowIds: p.HasRowIDs, RowIds: p.RowIDs, ByLeadIds: !p.HasRowIDs && p.HasLeadIDs, LeadIds: p.LeadIDs})
		return err
	})
	return wb, rows, err
}

// usesLegacyLeads ports cell_scope.uses_legacy_leads.
func usesLegacyLeads(wb gen.GetWorkbookForRunRow) bool {
	if !wb.SourceType.Valid || wb.SourceType.String != "leads_filter" {
		return false
	}
	cfg, err := jsonObject(wb.SourceConfig)
	if err != nil {
		return true
	}
	v, ok := cfg["row_storage_version"].(int64)
	return !ok || v != 2
}

// pausedOrCancelled is the body of _should_stop's DB read: the workbook was
// paused, or this run's job was cancelled.
func (rs *runState) pausedOrCancelled(ctx context.Context) bool {
	var status string
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		s, e := gen.New(tx).GetWorkbookStatus(ctx, gen.GetWorkbookStatusParams{WorkbookID: rs.p.WorkbookID, WorkspaceID: rs.p.WorkspaceID})
		status = s.String
		return e
	})
	if err == nil && status == "paused" {
		return true
	}
	if cancelled, cerr := queue.IsCancelled(ctx, rs.deps.Pool, rs.claim); cerr == nil && cancelled {
		return true
	}
	return false
}

type rowResult struct {
	completed, errors int
	err               error
}

// runOneRow ports _run_one_row: columns run sequentially, each produced value
// is threaded into the local copy of the row for later columns.
func (rs *runState) runOneRow(ctx context.Context, w workItem) rowResult {
	row := make(map[string]any, len(w.lead))
	for k, v := range w.lead {
		row[k] = v
	}
	identities := map[string]any{}
	for _, k := range []string{"id", "__row_id", "__lead_id"} {
		if v, ok := w.lead[k]; ok {
			identities[k] = v
		}
	}
	var r rowResult
	var failed []column
	for _, col := range w.cols {
		if rs.stop.shouldStop() {
			break
		}
		var out cellOutcome
		var err error
		if rs.upstreamFailed(col, failed) {
			// Never run a dependent against the old hydrated value after its
			// upstream attempt failed; persist the reason for reload/retry.
			out, err = rs.failDependent(ctx, row, col, "upstream_dependency_failed")
		} else {
			out, err = rs.enrichCell(ctx, row, col)
		}
		if err != nil {
			r.err = err
			return r
		}
		if out.Success {
			r.completed++
			var threaded any = out.Value
			if out.HasRaw {
				threaded = out.Raw
			}
			row[col.ID] = threaded
			if col.Name != "" {
				row[col.Name] = threaded
			}
			for k, v := range identities {
				row[k] = v
			}
		} else {
			r.errors++
			failed = append(failed, col)
			for _, key := range []string{col.ID, col.Name} {
				if _, isIdentity := identities[key]; !isIdentity {
					delete(row, key)
				}
			}
		}
	}
	return r
}

// runBatch runs up to Concurrency rows at once, like asyncio.gather. An abort
// error from any row cancels its siblings so in-flight paid calls end uncertain.
func (rs *runState) runBatch(ctx context.Context, batch []workItem) ([]rowResult, error) {
	gctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	results := make([]rowResult, len(batch))
	var wg sync.WaitGroup
	for i := range batch {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				// Backstop: nothing below should panic (cells recover on their
				// own), but a goroutine panic would kill the whole process.
				if r := recover(); r != nil {
					rs.log.Error("row panicked", "row", batch[i].rowID(), "stack", string(debug.Stack()))
					results[i] = rowResult{err: fmt.Errorf("panic: %v", truncRunes(fmt.Sprint(r), maxErrorRunes))}
					cancel(results[i].err)
				}
			}()
			results[i] = rs.runOneRow(gctx, batch[i])
			if results[i].err != nil {
				cancel(results[i].err)
			}
		}()
	}
	wg.Wait()
	for _, r := range results {
		if r.err != nil {
			return results, r.err
		}
	}
	return results, nil
}

// storedCells ports _stored_cells for v2 rows: {(rowID, columnID)} whose
// overlay status matches (complete additionally requires a non-null value).
func (rs *runState) storedCells(ctx context.Context, leads []map[string]any, status string) (map[cellKey]bool, error) {
	ids := make([]int32, 0, len(leads))
	for _, l := range leads {
		ids = append(ids, int32(intOf(l["__row_id"])))
	}
	found := map[cellKey]bool{}
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).ListRowEnrichments(ctx, gen.ListRowEnrichmentsParams{
			WorkbookID: rs.p.WorkbookID, WorkspaceID: rs.p.WorkspaceID, RowIds: ids})
		if err != nil {
			return err
		}
		for _, r := range rows {
			enr, err := jsonObject(r.Enrichments)
			if err != nil {
				return err
			}
			for cid, v := range enr {
				cell, ok := v.(map[string]any)
				if ok && cell["status"] == status && (status != "complete" || cell["value"] != nil) {
					found[cellKey{int64(r.ID), cid}] = true
				}
			}
		}
		return nil
	})
	return found, err
}

type cellKey struct {
	row int64
	col string
}

// restrictWorkItems ports cell_scope.restrict_work_items.
func restrictWorkItems(items []workItem, rc map[string][]string, restricted bool) []workItem {
	if !restricted {
		return items
	}
	var out []workItem
	for _, it := range items {
		allowed := map[string]bool{}
		for _, id := range rc[fmt.Sprint(it.rowID())] {
			allowed[id] = true
		}
		var kept []column
		for _, c := range it.cols {
			if allowed[c.ID] {
				kept = append(kept, c)
			}
		}
		if len(kept) > 0 {
			out = append(out, workItem{lead: it.lead, cols: kept})
		}
	}
	return out
}

func countCells(items []workItem) int {
	n := 0
	for _, it := range items {
		n += len(it.cols)
	}
	return n
}

// execute is the body of the try block in _run_workbook_enrichment_impl.
func (rs *runState) execute(ctx context.Context, ordered []column, leads []map[string]any) (Result, error) {
	var res Result
	p := rs.p
	ordered = orderColumns(ordered) // topo_sort_columns
	items := make([]workItem, 0, len(leads))
	if p.FillMissing {
		done, err := rs.storedCells(ctx, leads, "complete")
		if err != nil {
			return res, err
		}
		for _, l := range leads {
			var cols []column
			for _, c := range ordered {
				if !done[cellKey{intOf(l["__row_id"]), c.ID}] {
					cols = append(cols, c)
				}
			}
			if len(cols) > 0 {
				items = append(items, workItem{l, cols})
			}
		}
	} else {
		for _, l := range leads {
			items = append(items, workItem{l, ordered})
		}
	}
	items = restrictWorkItems(items, p.RowColumns, p.hasRowColumns)
	selected := map[cellKey]bool{}
	for _, it := range items {
		for _, c := range it.cols {
			selected[cellKey{it.rowID(), c.ID}] = true
		}
	}
	res.Total = countCells(items)
	rowsDone := 0

	// Cost lever: batch eligible AI cells before the per-row loop. Any failure
	// leaves the cells to the synchronous loop below.
	if !rs.stop.shouldStop() {
		handled, n, err := rs.aiBatchPrepass(ctx, items)
		if err != nil {
			return res, err
		}
		if len(handled) > 0 {
			items = applyBatchHandled(items, handled)
			res.Completed += n
		}
	}

	finish := func(err error) (Result, error) { return res, err }
	for i := 0; i < len(items); i += p.Concurrency {
		if rs.stop.shouldStop() {
			res.Stopped = true
			break
		}
		batch := items[i:min(i+p.Concurrency, len(items))]
		results, err := rs.runBatch(ctx, batch)
		rowsDone += len(batch)
		rs.rowsDone = rowsDone
		if err != nil {
			return finish(err)
		}
		for _, r := range results {
			res.Completed += r.completed
			res.Errors += r.errors
		}
		// Progress: completed_rows = rows fully processed so far.
		if err := tenancy.InWorkspace(ctx, rs.deps.Pool, p.WorkspaceID, func(tx pgx.Tx) error {
			if err := fence(ctx, tx, rs.claim, p.WorkbookID); err != nil {
				return err
			}
			return gen.New(tx).SetWorkbookProgress(ctx, gen.SetWorkbookProgressParams{
				TotalRows: int32(len(leads)), CompletedRows: int32(min(len(leads), rowsDone)),
				WorkbookID: p.WorkbookID, WorkspaceID: p.WorkspaceID})
		}); err != nil {
			return finish(err)
		}
	}

	// ── Retry passes: re-run only the cells still in `error` ──
	byRow := map[int64]map[string]any{}
	for _, l := range leads {
		byRow[intOf(l["__row_id"])] = l
	}
	for pass := 0; pass < max(0, p.RetryPasses); pass++ {
		if res.Stopped || rs.stop.shouldStop() {
			break
		}
		errCells, err := rs.storedCells(ctx, leads, "error")
		if err != nil {
			return finish(err)
		}
		errByRow := map[int64]map[string]bool{}
		for k := range errCells {
			if _, ok := byRow[k.row]; ok && selected[k] && !rs.isNoRetry(k) {
				if errByRow[k.row] == nil {
					errByRow[k.row] = map[string]bool{}
				}
				errByRow[k.row][k.col] = true
			}
		}
		if len(errByRow) == 0 {
			break
		}
		rowIDs := make([]int64, 0, len(errByRow))
		for id := range errByRow {
			rowIDs = append(rowIDs, id)
		}
		sort.Slice(rowIDs, func(i, j int) bool { return rowIDs[i] < rowIDs[j] })
		var targets []workItem
		for _, id := range rowIDs {
			var cols []column
			for _, c := range ordered {
				if errByRow[id][c.ID] { // allows_automatic_retry: all go column types
					cols = append(cols, c)
				}
			}
			targets = append(targets, workItem{byRow[id], cols})
		}
		targets = restrictWorkItems(targets, p.RowColumns, p.hasRowColumns)
		if len(targets) == 0 {
			break
		}
		rs.log.Info("retry pass", "pass", pass+1, "of", p.RetryPasses, "cells", countCells(targets))
		for i := 0; i < len(targets); i += p.Concurrency {
			if rs.stop.shouldStop() {
				res.Stopped = true
				break
			}
			results, err := rs.runBatch(ctx, targets[i:min(i+p.Concurrency, len(targets))])
			if err != nil {
				return finish(err)
			}
			for _, r := range results {
				res.Completed += r.completed
				res.Errors -= r.completed // moved error -> complete
			}
		}
	}
	return finish(nil)
}

// finalize ports the `finally` block: never leave the workbook stuck in
// running. It runs detached from ctx so a stop still lands, but only writes
// when the lease is still ours (a lost/cancelled lease owns no state), and
// leaves the status alone when the context was cancelled so a reclaimed run can
// resume instead of finding a stale `paused`.
func (rs *runState) finalize(ctx context.Context, res *Result, runErr error, nLeads int) {
	if runErr != nil && ctx.Err() != nil {
		res.Stopped = true
		return
	}
	failed := runErr != nil
	status := "complete"
	switch {
	case failed:
		status = "failed"
	case res.Stopped:
		status = "paused"
	case res.Errors > 0:
		status = "failed"
	}
	completedRows := nLeads
	if res.Stopped || failed {
		completedRows = min(nLeads, rs.rowsDone)
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
	defer cancel()
	owns := true
	err := tenancy.InWorkspace(fctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		if err := fence(fctx, tx, rs.claim, rs.p.WorkbookID); err != nil {
			return err
		}
		eff, err := gen.New(tx).FinalizeWorkbook(fctx, gen.FinalizeWorkbookParams{FinalStatus: status,
			CompletedRows: int32(completedRows), WorkbookID: rs.p.WorkbookID, WorkspaceID: rs.p.WorkspaceID})
		if err == nil && eff.String == "paused" {
			status = "paused"
		}
		return err
	})
	if errors.Is(err, ErrLeaseLost) {
		owns = false
	} else if err != nil {
		rs.log.Error("could not finalize workbook status", "err", err)
		owns = false
	}
	if owns {
		rs.publish(fctx, map[string]any{"type": "workbook_status", "status": status,
			"completed": res.Completed, "errors": res.Errors, "total": res.Total})
	}
}
