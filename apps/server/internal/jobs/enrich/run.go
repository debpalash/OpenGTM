package enrich

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
	"github.com/debpalash/OpenGTM/apps/server/internal/progress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// enrichmentColTypes is ENRICHMENT_COL_TYPES: the column types a workbook run
// executes. Only enrichment and waterfall columns are supported here; the
// Python route never sends a run containing any other type.
var enrichmentColTypes = []string{"enrichment", "waterfall", "ai_formula", "research", "agent", "http", "formula", "output"}

// UnsupportedError reports a payload this executor refuses to run. The Python
// route only enqueues the type for runs it can execute; this is the fail-safe
// for a payload that arrives some other way, so nothing runs unaccounted.
type UnsupportedError struct{ Reason string }

func (e *UnsupportedError) Error() string {
	return "run_workbook_connector cannot run this workbook: " + e.Reason + " (route the job type back to python)"
}

// runResult is the dictionary run_workbook_enrichment returns.
type runResult struct {
	completed, errors, total, rows int
	stopped                        bool
	// receipt reports whether Python would persist execution_result (the
	// empty-run and missing-workbook results lack the `stopped` key).
	receipt bool
}

type run struct {
	w          *Worker
	job        queue.Job
	pl         payload
	ws, wbID   string
	led        *ledger
	caller     *providerCaller
	allColumns []*column
	stop       *stopProbe
	rowCols    map[int64]map[string]bool // restrict_work_items allowlist, nil = unrestricted
}

func (r *run) runID() string { return fmt.Sprintf("job:%d", r.job.ID) }

type workItem struct {
	row  *workRow
	cols []*column
}

// loaded is the workbook state read at the start of a run.
type loaded struct {
	found      bool
	columns    []*column
	runColumns []*column
	rows       []*workRow
	enr        map[int64]*pycompat.Map // stored cells per row, for fill_missing
}

func isEnrichmentType(t string) bool { return slices.Contains(enrichmentColTypes, t) }

// load reads the workbook, its columns and the rows the run covers.
func (r *run) load(ctx context.Context) (loaded, error) {
	var out loaded
	err := db.WithTenant(ctx, r.w.pool, r.ws, func(tx pgx.Tx) error {
		var cfg, sourceType, sourceCfg *string
		err := tx.QueryRow(ctx, `SELECT columns_config::text, source_type, source_config::text FROM workbooks
			WHERE id = $1 AND workspace_id = $2`, r.wbID, r.ws).Scan(&cfg, &sourceType, &sourceCfg)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out.found = true
		if cfg != nil {
			v, err := pycompat.LoadJSON([]byte(*cfg))
			if err != nil {
				return fmt.Errorf("columns_config: %w", err)
			}
			list, _ := v.([]any)
			for _, e := range list {
				if c, ok := newColumn(e); ok {
					out.columns = append(out.columns, c)
				}
			}
		}
		for _, c := range out.columns {
			if isEnrichmentType(c.typ) && (r.pl.ColumnIDs == nil || slices.Contains(r.pl.ColumnIDs, c.id)) {
				out.runColumns = append(out.runColumns, c)
			}
		}

		var v2 int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workbook_rows WHERE workbook_id = $1`, r.wbID).Scan(&v2); err != nil {
			return err
		}
		if v2 == 0 {
			legacy := sourceType != nil && *sourceType == "leads_filter"
			if legacy && sourceCfg != nil {
				if v, err := pycompat.LoadJSON([]byte(*sourceCfg)); err == nil {
					if m, ok := v.(*pycompat.Map); ok {
						if ver, _ := m.Get("row_storage_version"); ver == int64(2) {
							legacy = false
						}
					}
				}
			}
			// _load_workbook_leads: a selected v2 scope never widens to legacy leads.
			if r.pl.RowIDs != nil || (r.pl.LeadIDs != nil && len(r.pl.LeadIDs) == 0) || !legacy {
				return nil
			}
			return &UnsupportedError{"legacy lead rows"}
		}

		query := `SELECT id, lead_id, data::text, enrichments::text FROM workbook_rows WHERE workbook_id = $1`
		args := []any{r.wbID}
		switch {
		case r.pl.RowIDs != nil:
			query += ` AND id = ANY($2::bigint[])`
			args = append(args, r.pl.RowIDs)
		case r.pl.LeadIDs != nil:
			query += ` AND lead_id = ANY($2::bigint[])`
			args = append(args, r.pl.LeadIDs)
		}
		rows, err := tx.Query(ctx, query+` ORDER BY id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		out.enr = map[int64]*pycompat.Map{}
		for rows.Next() {
			var id int64
			var leadID *int64
			var data, enr []byte
			if err := rows.Scan(&id, &leadID, &data, &enr); err != nil {
				return err
			}
			if leadID != nil {
				return &UnsupportedError{"workbook rows linked to leads"}
			}
			lead, err := hydrate(id, leadID, data, enr, out.columns)
			if err != nil {
				return err
			}
			em, err := decodeObject(enr)
			if err != nil {
				return err
			}
			out.enr[id] = em
			out.rows = append(out.rows, &workRow{id: id, lead: lead})
		}
		return rows.Err()
	})
	return out, err
}

// validate rejects anything outside the supported slice.
func (r *run) validate(cols []*column) error {
	for _, c := range cols {
		if c.typ != "enrichment" && c.typ != "waterfall" {
			return &UnsupportedError{"column " + c.id + " has type " + c.typ}
		}
		if pycompat.Truthy(mapGet(c.cfg, "condition")) {
			return &UnsupportedError{"column " + c.id + " has a condition"}
		}
		if hasReferences(c.cfg) {
			return &UnsupportedError{"column " + c.id + " references other columns"}
		}
		if c.targetFieldForVerify() == "email" {
			if v, ok := c.cfg.Get("verify"); !ok || pycompat.Truthy(v) {
				return &UnsupportedError{"column " + c.id + " verifies email addresses"}
			}
		}
		chain, selected := c.explicitChain()
		if !selected || len(chain) == 0 {
			return &UnsupportedError{"column " + c.id + " uses the default provider waterfall"}
		}
		for _, name := range chain {
			if r.w.plugin(name) == nil {
				return &UnsupportedError{"provider " + name + " is not a connector loaded by this worker"}
			}
		}
	}
	return nil
}

// hasReferences is column_deps._refs_in(...) != empty for the templated keys.
func hasReferences(cfg *pycompat.Map) bool {
	for _, k := range []string{"prompt", "formula", "http_url", "condition", "goal"} {
		if s, ok := mapGet(cfg, k).(string); ok && refRE.MatchString(s) {
			return true
		}
	}
	if list, ok := mapGet(cfg, "input_columns").([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok && pycompat.Strip(s) != "" {
				return true
			}
		}
	}
	for _, k := range []string{"http_headers", "http_body", "destination_config"} {
		if v, ok := cfg.Get(k); ok && v != nil && stringsHaveRef(v) {
			return true
		}
	}
	return false
}

func (r *run) execute(ctx context.Context) (runResult, error) {
	ld, err := r.load(ctx)
	if err != nil {
		return runResult{}, err
	}
	if !ld.found {
		r.w.log.Warn("workbook not found; nothing to run", "job_id", r.job.ID, "workbook_id", r.wbID)
		return runResult{}, nil
	}
	if err := r.validate(ld.runColumns); err != nil {
		return runResult{}, err
	}
	r.allColumns = ld.columns

	if len(ld.runColumns) == 0 || len(ld.rows) == 0 {
		err := r.setStatus(ctx, "complete", false)
		return runResult{rows: len(ld.rows)}, err
	}
	if err := r.beginRun(ctx); err != nil {
		return runResult{}, err
	}

	items := make([]workItem, 0, len(ld.rows))
	for _, row := range ld.rows {
		cols := ld.runColumns
		if r.pl.FillMissing {
			var gaps []*column
			for _, c := range cols {
				if !cellComplete(ld.enr[row.id], c.id) {
					gaps = append(gaps, c)
				}
			}
			if len(gaps) == 0 {
				continue
			}
			cols = gaps
		}
		items = append(items, workItem{row, cols})
	}
	items = r.restrict(items)
	selected := map[cellKey]bool{}
	total := 0
	for _, it := range items {
		total += len(it.cols)
		for _, c := range it.cols {
			selected[cellKey{it.row.id, c.id}] = true
		}
	}

	var (
		completed, failed, rowsDone int
		stopped, crashed            bool
		runErr                      error
	)
	var progressMu sync.Mutex
	progressAt := time.Now()
	onRow := func(done, completed, failed int) {
		progressMu.Lock()
		due := time.Since(progressAt) >= 250*time.Millisecond
		if due {
			progressAt = time.Now()
		}
		progressMu.Unlock()
		if due {
			// Best effort: a missed progress write never fails the run.
			_ = r.reportProgress(ctx, len(ld.rows), done, completed, failed)
		}
	}

	c, f, n, perr := r.runItems(ctx, items, r.pl.Force, onRow)
	completed, failed, rowsDone, runErr = c, f, n, perr
	if runErr == nil && rowsDone > 0 {
		// Python writes total_rows and completed_rows after every batch, so a run
		// that processed any row always leaves them set, however fast it was.
		runErr = r.reportProgress(ctx, len(ld.rows), rowsDone, completed, failed)
	}
	if r.stop.latched() {
		stopped = true
	}
	if runErr == nil && r.pl.RetryPasses > 0 {
		// Retry passes re-run the cells still in `error`, on the rows as they
		// were first hydrated (like Python), bounded by retry_passes.
		byRow := map[int64]*workRow{}
		for _, row := range ld.rows {
			byRow[row.id] = row
		}
		for pass := 0; pass < r.pl.RetryPasses; pass++ {
			if stopped || r.stop.should(ctx) {
				break
			}
			errCells, err := r.storedErrorCells(ctx, ld.rows)
			if err != nil {
				runErr = err
				break
			}
			var targets []workItem
			for _, id := range sortedRowIDs(errCells) {
				row := byRow[id]
				var cols []*column
				for _, c := range ld.runColumns {
					if errCells[id][c.id] && selected[cellKey{id, c.id}] {
						cols = append(cols, c)
					}
				}
				if row != nil && len(cols) > 0 {
					targets = append(targets, workItem{row, cols})
				}
			}
			targets = r.restrict(targets)
			if len(targets) == 0 {
				break
			}
			cells := 0
			for _, t := range targets {
				cells += len(t.cols)
			}
			r.w.log.Info("retry pass", "job_id", r.job.ID, "pass", pass+1, "of", r.pl.RetryPasses, "cells", cells)
			c2, _, _, perr := r.runItems(ctx, targets, false, nil)
			completed += c2
			failed -= c2 // moved from error to complete
			if perr != nil {
				runErr = perr
				break
			}
			if r.stop.latched() {
				stopped = true
				break
			}
		}
	}
	if runErr != nil {
		crashed = true
	}

	// An attempt that was cancelled, timed out or interrupted by shutdown
	// writes nothing more: the queue decides what happens to the job, exactly
	// as when Python's child process is killed.
	if ctx.Err() != nil || errors.Is(runErr, queue.ErrLeaseLost) {
		return runResult{}, firstErr(runErr, ctx.Err())
	}

	final := "complete"
	switch {
	case crashed:
		final = "failed"
	case stopped:
		final = "paused"
	case failed > 0:
		final = "failed"
	}
	doneRows := len(ld.rows)
	if stopped || crashed {
		doneRows = min(len(ld.rows), rowsDone)
	}
	owned, err := r.finalize(ctx, final, doneRows, completed, failed, total)
	if err != nil && runErr == nil {
		runErr = err
	}
	_ = owned
	res := runResult{completed: completed, errors: failed, total: total, rows: len(ld.rows), stopped: stopped, receipt: true}
	return res, runErr
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

type cellKey struct {
	row int64
	col string
}

func cellComplete(enr *pycompat.Map, colID string) bool {
	if enr == nil {
		return false
	}
	cm, ok := mapGet(enr, colID).(*pycompat.Map)
	if !ok {
		return false
	}
	s, _ := mapGet(cm, "status").(string)
	return s == "complete" && mapGet(cm, "value") != nil
}

// restrict is cell_scope.restrict_work_items: intersect with the allowlist;
// rows or columns absent from it mean no cells, never all cells.
func (r *run) restrict(items []workItem) []workItem {
	if r.pl.RowColumns == nil {
		return items
	}
	var out []workItem
	for _, it := range items {
		allowed := r.pl.RowColumns[it.row.id]
		var kept []*column
		for _, c := range it.cols {
			if slices.Contains(allowed, c.id) {
				kept = append(kept, c)
			}
		}
		if len(kept) > 0 {
			out = append(out, workItem{it.row, kept})
		}
	}
	return out
}

func sortedRowIDs(m map[int64]map[string]bool) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// storedErrorCells is _stored_cells(..., "error") over the run's rows.
func (r *run) storedErrorCells(ctx context.Context, rows []*workRow) (map[int64]map[string]bool, error) {
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.id
	}
	out := map[int64]map[string]bool{}
	err := db.WithTenant(ctx, r.w.pool, r.ws, func(tx pgx.Tx) error {
		q, err := tx.Query(ctx, `SELECT id, enrichments::text FROM workbook_rows
			WHERE workbook_id = $1 AND id = ANY($2::bigint[])`, r.wbID, ids)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var id int64
			var raw []byte
			if err := q.Scan(&id, &raw); err != nil {
				return err
			}
			m, err := decodeObject(raw)
			if err != nil {
				return err
			}
			for _, e := range m.Entries() {
				cm, ok := e.Value.(*pycompat.Map)
				if !ok {
					continue
				}
				if s, _ := mapGet(cm, "status").(string); s == "error" {
					if out[id] == nil {
						out[id] = map[string]bool{}
					}
					out[id][pycompat.Str(e.Key)] = true
				}
			}
		}
		return q.Err()
	})
	return out, err
}

// runItems runs rows with at most `concurrency` in flight, columns in order
// within a row. It returns the cell counts, the rows finished, and a non-nil
// error only when the attempt must stop.
func (r *run) runItems(ctx context.Context, items []workItem, force bool, onRow func(done, completed, failed int)) (completed, failed, rowsDone int, err error) {
	gctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sem := make(chan struct{}, r.pl.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, it := range items {
		if r.stop.should(gctx) {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-gctx.Done():
		}
		if gctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c, f, rerr := r.runRow(gctx, it, force)
			mu.Lock()
			completed += c
			failed += f
			rowsDone++
			done, comp, fail := rowsDone, completed, failed
			if rerr != nil && firstErr == nil {
				firstErr = rerr
				cancel(rerr)
			}
			mu.Unlock()
			if onRow != nil && rerr == nil {
				onRow(done, comp, fail)
			}
		}()
	}
	wg.Wait()
	return completed, failed, rowsDone, firstErr
}

// runRow is _run_one_row: columns sequentially, each success threaded into a
// local copy of the row so later columns see it.
func (r *run) runRow(ctx context.Context, it workItem, force bool) (completed, failed int, err error) {
	row := it.row.lead.Copy()
	identities := map[string]any{}
	for _, k := range []string{"id", "__row_id", "__lead_id"} {
		if v, ok := it.row.lead.Get(k); ok {
			identities[k] = v
		}
	}
	for _, col := range it.cols {
		if r.stop.should(ctx) {
			break
		}
		res, cerr := r.runCell(ctx, it.row.id, row, col)
		if cerr != nil {
			return completed, failed, cerr
		}
		if res.Success {
			completed++
			_ = row.Set(col.id, res.Value)
			if col.name != "" {
				_ = row.Set(col.name, res.Value)
			}
			for k, v := range identities {
				_ = row.Set(k, v)
			}
			continue
		}
		failed++
		for _, key := range []string{col.id, col.name} {
			if _, isIdentity := identities[key]; !isIdentity {
				row.Delete(key)
			}
		}
	}
	return completed, failed, nil
}

// stopProbe is the cooperative-stop signal: the workbook paused or the job no
// longer running, read at most every two seconds and latched once true.
type stopProbe struct {
	r      *run
	mu     sync.Mutex
	last   time.Time
	stoppd bool
}

const stopPollEvery = 2 * time.Second

func (s *stopProbe) latched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stoppd
}

func (s *stopProbe) should(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stoppd {
		return true
	}
	if ctx.Err() != nil {
		s.stoppd = true
		return true
	}
	if time.Since(s.last) < stopPollEvery {
		return false
	}
	s.last = time.Now()
	var wstatus, jstatus *string
	err := db.WithTenant(ctx, s.r.w.pool, s.r.ws, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM workbooks WHERE id = $1`, s.r.wbID).Scan(&wstatus); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, s.r.job.ID).Scan(&jstatus); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return nil
	})
	if err != nil {
		return false // an unreadable probe never stops a run
	}
	if (wstatus != nil && *wstatus == "paused") || (jstatus != nil && (*jstatus == "cancelled" || *jstatus == "failed")) {
		s.stoppd = true
	}
	return s.stoppd
}

// beginRun marks the workbook running (unless paused), under the lease.
func (r *run) beginRun(ctx context.Context) error {
	return db.WithTenant(ctx, r.w.pool, r.ws, func(tx pgx.Tx) error {
		if err := holdLease(ctx, tx, r.job); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workbooks SET status = 'running', updated_at = now()
			WHERE id = $1 AND workspace_id = $2 AND status IS DISTINCT FROM 'paused'`, r.wbID, r.ws); err != nil {
			return err
		}
		return progress.Publish(ctx, tx, r.ws, "workbook_status", map[string]any{"workbook_id": r.wbID, "status": "running"})
	})
}

// setStatus is the empty-run completion: fenced, and not for a paused workbook.
func (r *run) setStatus(ctx context.Context, status string, _ bool) error {
	return db.WithTenant(ctx, r.w.pool, r.ws, func(tx pgx.Tx) error {
		if err := holdLease(ctx, tx, r.job); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workbooks SET status = $3, updated_at = now() WHERE id = $1 AND workspace_id = $2`,
			r.wbID, r.ws, status)
		return err
	})
}

// reportProgress mirrors the per-batch workbooks.completed_rows update.
func (r *run) reportProgress(ctx context.Context, total, done, completed, failed int) error {
	return db.WithTenant(ctx, r.w.pool, r.ws, func(tx pgx.Tx) error {
		if err := holdLease(ctx, tx, r.job); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workbooks SET total_rows = $3, completed_rows = $4, updated_at = now()
			WHERE id = $1 AND workspace_id = $2`, r.wbID, r.ws, total, min(total, done)); err != nil {
			return err
		}
		return progress.Publish(ctx, tx, r.ws, "workbook_run_progress", map[string]any{
			"workbook_id": r.wbID, "rows_done": done, "total_rows": total, "completed": completed, "errors": failed,
		})
	})
}

// finalize writes the terminal workbook status, never leaving it `running`.
// It runs on a detached context so a stop or timeout still records the state,
// and it writes nothing if the lease is gone or the workbook is paused.
func (r *run) finalize(ctx context.Context, status string, doneRows, completed, failed, total int) (bool, error) {
	bctx, cancel := detached(ctx)
	defer cancel()
	owned := true
	err := db.WithTenant(bctx, r.w.pool, r.ws, func(tx pgx.Tx) error {
		if err := holdLease(bctx, tx, r.job); err != nil {
			if errors.Is(err, queue.ErrLeaseLost) {
				owned = false
				return nil
			}
			return err
		}
		var current *string
		if err := tx.QueryRow(bctx, `SELECT status FROM workbooks WHERE id = $1 AND workspace_id = $2`, r.wbID, r.ws).
			Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if current == nil || *current != "paused" {
			if _, err := tx.Exec(bctx, `UPDATE workbooks SET status = $3, completed_rows = $4, updated_at = now()
				WHERE id = $1 AND workspace_id = $2`, r.wbID, r.ws, status, doneRows); err != nil {
				return err
			}
		} else {
			status = "paused"
		}
		return progress.Publish(bctx, tx, r.ws, "workbook_status", map[string]any{
			"workbook_id": r.wbID, "status": status, "completed": completed, "errors": failed, "total": total,
		})
	})
	return owned, err
}

// persistReceipt is run_receipts.persist_run_result: the execution_result the
// /runs endpoint serves, written only for the lease that produced it.
func (r *run) persistReceipt(ctx context.Context, res runResult) error {
	bctx, cancel := detached(ctx)
	defer cancel()
	_, err := r.w.pool.Exec(bctx, `UPDATE jobs SET payload = (payload::jsonb || jsonb_build_object('execution_result',
			jsonb_build_object('completed', $6::int, 'errors', $7::int, 'total', $8::int, 'rows', $9::int,
				'stopped', $10::boolean, 'recorded_at', $11::text, 'retry_count', coalesce(retry_count, 0))))::json
		WHERE id = $1 AND type = $2 AND workspace_id = $3 AND worker_id = $4 AND locked_at = $5
		  AND status IN ('processing', 'cancelled') AND payload->>'workbook_id' = $12`,
		r.job.ID, JobType, r.ws, r.job.Lease.WorkerID, r.job.Lease.LockedAt,
		res.completed, res.errors, res.total, res.rows, res.stopped,
		time.Now().UTC().Format("2006-01-02T15:04:05.000000")+"+00:00", r.wbID)
	return err
}
