package workbook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// In-request single-cell run (POST .../rows/{row_id}/cells/{col_id}/run). It runs the very
// same cell code as the queued run_workbook executor (enrichCell and everything below it),
// minus the queue lease: Python's run_cell calls enrich_cell with no batch owner, so every
// lease fence is a no-op and no broadcast is made (redis_client=None).

// Sentinels the route maps to its answers.
var (
	ErrWorkbookNotFound  = errors.New("workbook: workbook not found")
	ErrColumnNotFound    = errors.New("workbook: enrichment column not found")
	ErrRowNotFound       = errors.New("workbook: workbook row not found")
	ErrLegacyRowNotFound = errors.New("workbook: legacy lead row not found") // "Row not found"
	// ErrNeedsPython marks a cell this process does not execute (output columns, email
	// verification, columns whose Python worker is unavailable): the route proxies the
	// request to the Python API unchanged. See NeedsPythonReason.
	ErrNeedsPython = errors.New("workbook: cell runs on the python api")
	// ErrClientGone means the caller went away before any paid call was dispatched; nothing ran.
	ErrClientGone = errors.New("workbook: client went away before dispatch")
)

// cellRunBudget bounds a detached in-request run (provider calls carry their own timeouts).
const cellRunBudget = 5 * time.Minute

type needsPythonError struct{ reason string }

func (e *needsPythonError) Error() string {
	return "workbook: cell runs on the python api: " + e.reason
}
func (e *needsPythonError) Is(t error) bool { return t == ErrNeedsPython }

// NeedsPythonReason is the reason carried by an ErrNeedsPython error.
func NeedsPythonReason(err error) string {
	var n *needsPythonError
	if errors.As(err, &n) {
		return n.reason
	}
	return ""
}

// CellRequest is one in-request cell run.
type CellRequest struct {
	WorkspaceID, WorkbookID string
	RowID                   int64
	ColumnID                string
	// Force bypasses the output run-once guard (output columns run on Python; the guard is
	// answered here only when Force is false).
	Force bool
	// ProviderTimeout bounds one provider call (Python _RUN_CONFIG provider_timeout, 10s).
	ProviderTimeout time.Duration
}

// CellResult is the route's answer: run_cell's dict.
type CellResult struct {
	Success, Skipped, Forced bool
	Value                    any
	Provider, Error          *string
	Research                 any
}

// Status is "skipped", "complete" or "error".
func (r CellResult) Status() string {
	switch {
	case r.Skipped:
		return "skipped"
	case r.Success:
		return "complete"
	}
	return "error"
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// MarshalJSON writes run_cell's keys in Python's order.
func (r CellResult) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	put := func(k string, v any, first bool) error {
		if !first {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		// Values are decoded stored JSON (json.Number keeps Python's number text).
		var vb bytes.Buffer
		enc := json.NewEncoder(&vb)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("workbook: cell result %s: %w", k, err)
		}
		buf.Write(bytes.TrimSuffix(vb.Bytes(), []byte("\n")))
		return nil
	}
	for i, kv := range []struct {
		k string
		v any
	}{{"status", r.Status()}, {"value", r.Value}, {"provider", r.Provider}, {"error", r.Error},
		{"skipped", r.Skipped}, {"forced", r.Forced}, {"research", r.Research}} {
		if err := put(kv.k, kv.v, i == 0); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// cellIdentity names the spend run of an in-request cell run. With a live run_workbook job
// of the same workbook the cell reserves under that job's run id and attempt keys, so a race
// with the queued run settles (and pays) once; without one it gets a run of its own.
type cellIdentity struct {
	keyID int64  // the "job id" inside the attempt key
	runID string // workbook_spend_attempts.run_id
	live  bool   // keyed by a live run_workbook job
}

// clientErr reports a client that left before dispatch (nil for a queued run). Once any paid
// call has been dispatched the run is detached from the client: the cell is finished, so no
// later provider is skipped (and left unpaid-but-half-done) because the caller disconnected.
func (rs *runState) clientErr() error {
	if rs.paidDispatched.Load() {
		return nil
	}
	if rs.client != nil && rs.client.Err() != nil {
		return fmt.Errorf("%w: %v", ErrClientGone, context.Cause(rs.client))
	}
	return nil
}

// resolveLiveJob keys the cell's spend to the workbook's live run_workbook job, if any.
func (rs *runState) resolveLiveJob(ctx context.Context, tx pgx.Tx) error {
	id, err := gen.New(tx).ActiveRunWorkbookJobID(ctx, gen.ActiveRunWorkbookJobIDParams{WorkspaceID: rs.p.WorkspaceID, WorkbookID: rs.p.WorkbookID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rs.cell = &cellIdentity{keyID: int64(id), runID: fmt.Sprintf("job:%d", id), live: true}
	return nil
}

func (rs *runState) spendJobID() int64 {
	if rs.claim != nil {
		return rs.claim.ID
	}
	return rs.cell.keyID
}

func (rs *runState) spendRunID() string {
	if rs.claim != nil {
		return fmt.Sprintf("job:%d", rs.claim.ID)
	}
	return rs.cell.runID
}

// leaseFence is the dispatch fence of a queued run; an in-request cell run has none.
func (rs *runState) leaseFence() *spend.LeaseFence {
	if rs.claim == nil {
		return nil
	}
	return &spend.LeaseFence{JobID: rs.claim.ID, WorkerID: rs.claim.WorkerID, LockedAt: rs.claim.LockedAt, LeaseToken: rs.claim.LeaseToken}
}

// loadedCell is everything EnrichCell reads before running.
type loadedCell struct {
	columns []column
	col     column
	lead    map[string]any
	row     *gen.ListWorkbookRowsRow // nil for a legacy lead-store row
	leadID  int64
	jobID   int64 // live run_workbook job (0 = none)
}

// EnrichCell runs one cell in the caller's request. A returned error is one of the
// sentinels above, a cancellation (ctx), or infrastructure.
func EnrichCell(ctx context.Context, deps Deps, req CellRequest) (CellResult, error) {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	timeout := req.ProviderTimeout
	if timeout <= 0 {
		timeout = time.Duration(defaultProviderTimeout * float64(time.Second))
	}
	timeout = min(timeout, time.Duration(MaxProviderTimeout*float64(time.Second)))
	lc, err := loadCell(ctx, deps, req)
	if err != nil {
		return CellResult{}, err
	}
	p := &runPayload{WorkspaceID: req.WorkspaceID, WorkbookID: req.WorkbookID, Concurrency: 1,
		ProviderTimeout: timeout.Seconds(), Manifests: map[string]manifest.Manifest{}}
	rs := &runState{deps: deps, p: p, pl: planner{catalog: deps.Runner != nil}, log: log}
	rs.runner, rs.legacy = deps.Runner, deps.Runner == nil
	if rs.legacy {
		rs.runner = manifestRunner{rs}
	}
	rs.cols = lc.columns
	rs.allCols = make([]colgraph.Col, len(lc.columns))
	for i, c := range lc.columns {
		rs.allCols[i] = c.g
	}
	rs.cycleBlocked = colgraph.CycleBlocked(rs.allCols)
	if lc.jobID > 0 {
		rs.cell = &cellIdentity{keyID: lc.jobID, runID: fmt.Sprintf("job:%d", lc.jobID), live: true}
	} else {
		id := time.Now().UnixNano() // far above any int4 job id: never collides with a queued run
		rs.cell = &cellIdentity{keyID: id, runID: fmt.Sprintf("cell:%d", id)}
	}
	// Detach: past the first paid dispatch the client's disconnect must not strand the attempt
	// `uncertain` or lose the provider result. The client's ctx still gates every pre-dispatch step.
	rs.client = ctx
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cellRunBudget)
	defer cancel()
	if err := rs.clientErr(); err != nil {
		return CellResult{}, err
	}
	return rs.runOneCell(dctx, lc, req.Force)
}

func loadCell(ctx context.Context, deps Deps, req CellRequest) (lc loadedCell, err error) {
	err = tenancy.InWorkspace(ctx, deps.Pool, req.WorkspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		wb, err := q.GetWorkbookForCellRun(ctx, gen.GetWorkbookForCellRunParams{WorkbookID: req.WorkbookID, WorkspaceID: req.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkbookNotFound
		}
		if err != nil {
			return err
		}
		if lc.columns, err = parseColumns(wb.ColumnsConfig); err != nil {
			return err
		}
		found := false
		for _, c := range lc.columns {
			if c.ID == req.ColumnID && enrichmentColTypes[c.Type] {
				lc.col, found = c, true
				break
			}
		}
		if !found {
			return ErrColumnNotFound
		}
		// Never reinterpret a missing v2 row identity as a linked lead identity.
		var rows []gen.ListWorkbookRowsRow
		if req.RowID > 0 && req.RowID <= math.MaxInt32 {
			if rows, err = q.ListWorkbookRows(ctx, gen.ListWorkbookRowsParams{WorkbookID: req.WorkbookID, WorkspaceID: req.WorkspaceID,
				ByRowIds: true, RowIds: []int32{int32(req.RowID)}}); err != nil {
				return err
			}
		}
		if len(rows) == 0 {
			n, err := q.CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: req.WorkbookID, WorkspaceID: req.WorkspaceID})
			if err != nil {
				return err
			}
			if n > 0 || !legacyLeadsWorkbook(wb.SourceType, wb.SourceConfig) {
				return ErrRowNotFound
			}
			if lc.lead, lc.leadID, err = legacyLead(ctx, tx, wb.FilterCriteria, req.RowID); err != nil {
				return err
			}
		} else {
			r := rows[0]
			lc.row = &r
			var lid *int64
			if r.LeadID.Valid {
				v := int64(r.LeadID.Int32)
				lid = &v
			}
			if lc.lead, err = rowExecutionData(int64(r.ID), lid, r.Data, r.Enrichments, lc.columns); err != nil {
				return err
			}
			lc.leadID = intOf(lc.lead["id"])
		}
		id, err := q.ActiveRunWorkbookJobID(ctx, gen.ActiveRunWorkbookJobIDParams{WorkspaceID: req.WorkspaceID, WorkbookID: req.WorkbookID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			lc.jobID = int64(id)
		}
		return nil
	})
	return lc, err
}

// legacyLead resolves a v1 workbook's row id in the tenant lead store (run_cell:
// `_query_leads(lead_db, {**filter_criteria, "lead_ids": [row_id]}, page=1, page_size=1)`).
func legacyLead(ctx context.Context, tx pgx.Tx, filterJSON []byte, rowID int64) (map[string]any, int64, error) {
	var f leadsread.Filter
	if len(filterJSON) > 0 && string(filterJSON) != "null" {
		if err := json.Unmarshal(filterJSON, &f); err != nil {
			return nil, 0, ErrLegacyRowNotFound // unreadable criteria never widen the lookup
		}
	}
	f.LeadIDs = []int64{rowID}
	leads, _, err := leadsread.QueryLeads(ctx, tx, f, 1, 1)
	if err != nil {
		return nil, 0, err
	}
	if len(leads) == 0 {
		return nil, 0, ErrLegacyRowNotFound
	}
	raw, err := json.Marshal(leads[0])
	if err != nil {
		return nil, 0, err
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		return nil, 0, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, 0, errors.New("workbook: lead is not an object")
	}
	id := leads[0].ID
	if id == 0 {
		id = rowID
	}
	m["id"] = id
	return m, id, nil
}

// legacyLeadsWorkbook ports cell_scope.uses_legacy_leads.
func legacyLeadsWorkbook(sourceType pgtype.Text, sourceConfig []byte) bool {
	if !sourceType.Valid || sourceType.String != "leads_filter" {
		return false
	}
	cfg, err := jsonObject(sourceConfig)
	if err != nil {
		return true
	}
	v, ok := cfg["row_storage_version"].(int64)
	return !ok || v != 2
}

// needsPython names the reason a column cannot run in this process ("" = it can).
func (rs *runState) needsPython(col column) string {
	if col.Type == "output" {
		return "output column"
	}
	if (col.Type == "enrichment" || col.Type == "waterfall") && col.firstTruthy("target_field", "lead_field") == "email" {
		if v, ok := col.cfg["verify"]; !ok || manifest.PyTruthy(v) {
			return "email verification"
		}
	}
	if err := rs.checkPyColumn(col); err != nil {
		reason, _ := refusalReason(err)
		return reason
	}
	return ""
}

func (rs *runState) outcomeResult(o cellOutcome) CellResult {
	r := CellResult{Success: o.Success, Provider: strPtr(o.Provider), Error: strPtr(o.Err), Research: o.Research}
	switch {
	case o.HasRaw:
		r.Value = o.Raw
	case o.Success:
		r.Value = o.Value
	}
	return r
}

// runOneCell is enrich_cell for one in-request cell: dependency and condition gates, the
// output run-once guard, then the shared cell code.
func (rs *runState) runOneCell(ctx context.Context, lc loadedCell, force bool) (CellResult, error) {
	col, lead := lc.col, lc.lead
	res := func(o cellOutcome, err error) (CellResult, error) {
		if err != nil {
			return CellResult{}, err
		}
		r := rs.outcomeResult(o)
		r.Forced = force
		return r, nil
	}
	if derr := rs.dependencyError(lead, col); derr != "" {
		return res(rs.failDependent(ctx, lead, col, derr))
	}
	if cond := col.cfg["condition"]; manifest.PyTruthy(cond) {
		if !colgraph.EvalConditionCols(cond, rowForColumn(lead), rs.allCols) {
			err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
				return setEnrichment(ctx, tx, cellWrite{WorkspaceID: rs.p.WorkspaceID, WorkbookID: rs.p.WorkbookID,
					LeadID: int32(lc.leadID), RowID: int32(intOf(lead["__row_id"])), ColumnID: col.ID, Status: "skipped"})
			})
			if err != nil {
				return CellResult{}, err
			}
			return res(cellOutcome{Err: "condition_not_met"}, nil)
		}
	}
	if col.Type == "output" {
		runOnce := true
		if v, ok := col.cfg["run_once"]; ok {
			runOnce = manifest.PyTruthy(v)
		}
		if runOnce && !force {
			if r, answered, err := rs.outputGuard(ctx, lc); err != nil {
				return CellResult{}, err
			} else if answered {
				r.Forced = force
				return r, nil
			}
		}
	}
	if reason := rs.needsPython(col); reason != "" {
		return CellResult{}, &needsPythonError{reason: reason}
	}
	return res(rs.enrichCell(ctx, lead, col))
}

// outputGuard answers the output run-once gate (a complete cell is never re-pushed without
// force). answered=false means the output must really run, which only Python does.
func (rs *runState) outputGuard(ctx context.Context, lc loadedCell) (CellResult, bool, error) {
	col := lc.col
	var prior gen.GetEnrichmentForOutputGuardRow
	var havePrior bool
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		prior, e = gen.New(tx).GetEnrichmentForOutputGuard(ctx, gen.GetEnrichmentForOutputGuardParams{
			WorkbookID: rs.p.WorkbookID, WorkspaceID: rs.p.WorkspaceID, LeadID: int32(lc.leadID), ColumnID: col.ID})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		havePrior = e == nil
		return e
	})
	if err != nil {
		return CellResult{}, false, err
	}
	priorComplete := havePrior && prior.Status.String == "complete"
	if lc.row == nil { // legacy lead-store row: the lead-keyed receipt is the only evidence
		if priorComplete {
			return CellResult{Success: true, Skipped: true, Value: textValue(prior.Value), Provider: textPtr(prior.Provider)}, true, nil
		}
		return CellResult{}, false, nil
	}
	enr, err := jsonObject(lc.row.Enrichments)
	if err != nil {
		return CellResult{}, false, err
	}
	switch own := enr[col.ID].(type) {
	case map[string]any:
		if own["status"] == "complete" {
			var prov *string
			if s, ok := own["provider"].(string); ok {
				prov = &s
			}
			return CellResult{Success: true, Skipped: true, Value: own["value"], Provider: prov}, true, nil
		}
	case nil:
		if priorComplete {
			// Old lead-keyed receipts cannot prove which v2 row was sent: never resend silently.
			e := "output_identity_requires_review"
			return CellResult{Error: &e}, true, nil
		}
	}
	return CellResult{}, false, nil
}

func textValue(t pgtype.Text) any {
	if !t.Valid {
		return nil
	}
	return t.String
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}
