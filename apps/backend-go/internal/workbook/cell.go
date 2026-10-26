package workbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const (
	markUncertainTimeout = 5 * time.Second
	maxErrorRunes        = 200
)

// cellOutcome is enrich_cell's return value, reduced to what the row loop uses.
type cellOutcome struct {
	Success bool
	Value   string
}

type callKind int

const (
	callOK        callKind = iota
	callReject             // reserve/dispatch refused: reason is the cell error
	callTimeout            // runner deadline (asyncio.TimeoutError)
	callException          // any other failure after/around the call
	callInvalid            // spend validation (programming) error; never "uncertain"
	callUnknown            // the runner does not know the provider: skipped
)

// providerCall is the outcome of one provider attempt.
type providerCall struct {
	kind callKind
	resp map[string]any // provider response dict (callOK)
	text string
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// abortErr reports a stop condition that must unwind the whole run: the job
// context was cancelled (lease loss, cancellation, shutdown) or the lease
// fence failed. Cell-local failures are never abort errors.
func abortErr(ctx context.Context, err error) error {
	if errors.Is(err, ErrLeaseLost) {
		return err
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

func (rs *runState) credFunc(ctx context.Context) func(string) (string, error) {
	if rs.deps.Creds == nil {
		return nil
	}
	return func(env string) (string, error) { return rs.deps.Creds.Get(ctx, rs.p.WorkspaceID, env) }
}

// runProvider runs one provider through the run's provider runner under the
// run's provider deadline. timedOut is true only when that deadline (not the
// manifest's own timeout, nor job cancellation) expired: Python raises
// asyncio.TimeoutError only then. A returned error is infrastructure (job
// cancelled, runner unavailable, unknown provider, credential lookup).
func (rs *runState) runProvider(ctx context.Context, name string, lead map[string]any, target string) (out providers.Output, timedOut bool, err error) {
	in := providers.Input{Lead: providerLead(lead), Field: target}
	out, err = rs.runner.Run(ctx, rs.p.WorkspaceID, name, in, time.Duration(rs.p.ProviderTimeout*float64(time.Second)))
	if err != nil {
		return providers.Output{}, false, err
	}
	return out, out.Error == "timeout", nil
}

// providerLead is the lead dict a provider sees: the hydrated row without the
// executor's own "__" bookkeeping keys.
func providerLead(lead map[string]any) map[string]any {
	out := make(map[string]any, len(lead))
	for k, v := range lead {
		if !strings.HasPrefix(k, "__") {
			out[k] = v
		}
	}
	return out
}

// prechecker is implemented by runners that can refuse a call before any
// network I/O (a missing credential, a blocked URL): such a call must never
// create a spend attempt.
type prechecker interface {
	precheck(ctx context.Context, name string, lead map[string]any) string
}

// responseDict is the dict run_provider returns for a provider call.
func responseDict(name string, out providers.Output) map[string]any {
	fields := map[string]any{}
	if out.Extra != nil {
		for _, k := range out.Extra.Keys() {
			v, _ := out.Extra.Get(k)
			fields[k] = v
		}
	}
	return map[string]any{
		"provider": name, "success": out.Error == "" && len(fields) > 0, "fields": fields, "confidence": out.Confidence,
		"error": out.Error, "billing_evidence": nil, "license": "unknown",
	}
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	}
	return 0
}

func (rs *runState) markUncertain(ctx context.Context, id spend.Identity) {
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markUncertainTimeout)
	defer cancel()
	err := tenancy.InWorkspace(mctx, rs.deps.Pool, id.WorkspaceID, func(tx pgx.Tx) error {
		_, e := spend.Transition(mctx, tx, id, "mark_uncertain")
		return e
	})
	if err != nil {
		rs.log.Error("could not mark spend attempt uncertain", "attempt_id", id.AttemptID, "err", err)
	}
}

// reservedCall ports execute_reserved_attempt for a paid provider: durable
// admission, fenced dispatch, the provider call, then settlement. Any failure
// after dispatch leaves the attempt `uncertain`. No network I/O happens inside
// a transaction.
func (rs *runState) reservedCall(ctx context.Context, name string, lead map[string]any,
	col column, target, rowIdentity string, exposure, cellLimit int64) (pcall providerCall, perr error) {
	ws, wb := rs.p.WorkspaceID, rs.p.WorkbookID
	resv := rs.buildReservation(name, lead, col, rowIdentity, exposure, cellLimit)
	var adm spend.Admission
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		var e error
		adm, e = spend.Reserve(ctx, tx, resv)
		return e
	})
	if err != nil {
		if a := abortErr(ctx, err); a != nil {
			return providerCall{}, a
		}
		if errors.Is(err, spend.ErrInvalidInput) {
			rs.log.Error("spend reservation rejected as invalid", "provider", name, "err", err)
			return providerCall{kind: callInvalid, text: truncRunes(err.Error(), maxErrorRunes)}, nil
		}
		return providerCall{kind: callException, text: truncRunes(err.Error(), maxErrorRunes)}, nil
	}
	if !adm.OK {
		return providerCall{kind: callReject, text: adm.Reason}, nil
	}
	id := spend.Identity{WorkspaceID: ws, WorkbookID: wb, AttemptID: adm.ID, ContractHash: adm.ContractHash,
		Lease: &spend.LeaseFence{JobID: rs.claim.ID, WorkerID: rs.claim.WorkerID, LockedAt: rs.claim.LockedAt, LeaseToken: rs.claim.LeaseToken}}

	if adm.Status == "settled" {
		return rs.replaySettled(ctx, id)
	}
	var dispatched bool
	err = tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		var e error
		dispatched, e = spend.Transition(ctx, tx, id, "dispatch")
		return e
	})
	if err != nil {
		if a := abortErr(ctx, err); a != nil {
			return providerCall{}, a
		}
		return providerCall{kind: callException, text: truncRunes(err.Error(), maxErrorRunes)}, nil
	}
	if !dispatched {
		return providerCall{kind: callReject, text: "attempt_not_dispatchable"}, nil
	}

	// From here the attempt is dispatched: a panic must leave it `uncertain`
	// (it may have reached the provider) and fail only this cell.
	defer func() {
		if r := recover(); r != nil {
			rs.log.Error("paid provider call panicked", "provider", name, "stack", string(debug.Stack()))
			rs.markUncertain(ctx, id)
			pcall, perr = providerCall{kind: callException, text: truncRunes(fmt.Sprintf("panic: %v", r), maxErrorRunes)}, nil
		}
	}()
	out, timedOut, rerr := rs.runProvider(ctx, name, lead, target)
	if ctx.Err() != nil {
		rs.markUncertain(ctx, id)
		return providerCall{}, context.Cause(ctx)
	}
	if rerr != nil {
		// The call was dispatched: whatever the runner reports, it may have
		// reached the vendor, so the attempt stays `uncertain`.
		rs.markUncertain(ctx, id)
		if errors.Is(rerr, pyworker.ErrUnknownProvider) {
			return providerCall{kind: callUnknown}, nil
		}
		if errors.Is(rerr, pyworker.ErrUnavailable) || errors.Is(rerr, pyworker.ErrUnauthorized) {
			return providerCall{}, fmt.Errorf("workbook: python worker: %w", rerr)
		}
		return providerCall{kind: callException, text: truncRunes(rerr.Error(), maxErrorRunes)}, nil
	}
	if timedOut {
		rs.markUncertain(ctx, id)
		return providerCall{kind: callTimeout}, nil
	}
	resp := responseDict(name, out)
	env, err := spend.AccountingEnvelope(name, spend.ProviderResponse(resp), exposure)
	if err != nil {
		rs.markUncertain(ctx, id)
		kind := callException
		if errors.Is(err, spend.ErrInvalidInput) {
			kind = callInvalid
		}
		return providerCall{kind: kind, text: truncRunes(err.Error(), maxErrorRunes)}, nil
	}
	var settled spend.SettleResult
	err = tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		var e error
		settled, e = spend.Settle(ctx, tx, id, env.ChargedMicroUSD, env.AccountingBasis, env.Result)
		return e
	})
	if err != nil || !settled.OK {
		rs.markUncertain(ctx, id)
		if a := abortErr(ctx, err); a != nil {
			return providerCall{}, a
		}
		text := "Provider outcome settlement was not confirmed"
		kind := callException
		if err != nil {
			text = truncRunes(err.Error(), maxErrorRunes)
			if errors.Is(err, spend.ErrInvalidInput) {
				kind = callInvalid
			}
		}
		return providerCall{kind: kind, text: text}, nil
	}
	return providerCall{kind: callOK, resp: env.Result}, nil
}

// providerInputs is the provider-visible part of the RAW stored row, used as
// the spend operation contract (Python: declarative_operation_contract). It must
// not come from the hydrated/threaded row: that also holds this run's own cell
// values, and a column whose id or name equals a lead field (email, phone, ...)
// would feed its output into later contracts, so a reclaimed run would see
// contract_conflict on cells it already finished.
func providerInputs(lead map[string]any) map[string]any {
	if base, ok := lead[contractInputsKey].(map[string]any); ok {
		out := make(map[string]any, len(base))
		for k, v := range base {
			out[k] = v
		}
		return out
	}
	in := leadStrings(lead)
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// operationInputs is the `inputs` of the spend operation contract: for a
// declarative provider only the manifest-readable fields of the raw row, for
// any other provider the whole hydrated row (Python _operation_contract).
func (rs *runState) operationInputs(name string, lead map[string]any) map[string]any {
	if _, declarative := rs.p.Manifests[name]; declarative {
		return providerInputs(lead)
	}
	out := make(map[string]any, len(lead))
	for k, v := range lead {
		if k != contractInputsKey {
			out[k] = v
		}
	}
	return out
}

// buildReservation is the spend reservation (and therefore contract hash) for
// one provider call. The Python executor builds the same operation contract
// for declarative providers (enrichment.declarative_operation_contract).
func (rs *runState) buildReservation(name string, lead map[string]any, col column, rowIdentity string, exposure, cellLimit int64) spend.Reservation {
	ws, wb := rs.p.WorkspaceID, rs.p.WorkbookID
	return spend.Reservation{
		WorkspaceID: ws, WorkbookID: wb, RunID: fmt.Sprintf("job:%d", rs.claim.ID), RowIdentity: rowIdentity,
		ColumnID: col.ID, Provider: name, AttemptKey: spend.AttemptKey(ws, wb, rs.claim.ID, rowIdentity, col.ID, name),
		ExposureMicroUSD: exposure, CellLimitMicroUSD: cellLimit,
		CostBasis: map[string]any{"kind": "catalog_estimate", "provider": name},
		Operation: map[string]any{"inputs": rs.operationInputs(name, lead), "column": col.cfg},
	}
}

// replaySettled returns the stored provider response of an already settled
// attempt (a crash-resume or retry pass) without calling the provider again.
func (rs *runState) replaySettled(ctx context.Context, id spend.Identity) (providerCall, error) {
	var stored []byte
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, id.WorkspaceID, func(tx pgx.Tx) error {
		row, e := gen.New(tx).GetAttemptResult(ctx, gen.GetAttemptResultParams{
			ID: id.AttemptID, WorkspaceID: id.WorkspaceID, WorkbookID: id.WorkbookID, ContractHash: id.ContractHash})
		stored = row.Result
		if e == nil && row.Status != "settled" {
			e = errors.New("attempt is not settled")
		}
		return e
	})
	if err != nil {
		if a := abortErr(ctx, err); a != nil {
			return providerCall{}, a
		}
		return providerCall{kind: callException, text: truncRunes(err.Error(), maxErrorRunes)}, nil
	}
	v, err := canonjson.Decode(stored)
	if m, ok := v.(map[string]any); err == nil && ok {
		if resp, ok := m["result"].(map[string]any); ok {
			return providerCall{kind: callOK, resp: resp}, nil
		}
	}
	return providerCall{kind: callException, text: "stored settlement result is malformed"}, nil
}

// enrichCell ports the waterfall branch of enrich_cell. A non-nil error means
// the whole run must unwind (lease lost or job context cancelled); cell-local
// failures are persisted as error cells and returned as a failed outcome.
func (rs *runState) enrichCell(ctx context.Context, lead map[string]any, col column) (cellOutcome, error) {
	rowID, leadID := intOf(lead["__row_id"]), intOf(lead["id"])
	wb := rs.p.WorkbookID

	// _run_one_cell: ownership preflight in its own short transaction.
	if err := checkOwner(ctx, rs.deps.Pool, rs.claim, wb); err != nil {
		return cellOutcome{}, err
	}
	// A dependency error returns before Python's "running" broadcast.
	if derr := rs.dependencyError(lead, col); derr != "" {
		return rs.failDependent(ctx, lead, col, derr)
	}
	// "running" is broadcast only: Python writes it in the same uncommitted
	// session that the result then overwrites.
	rs.publish(ctx, map[string]any{"type": "cell_update", "leadId": leadID, "rowId": rowID,
		"colId": col.ID, "status": "running", "value": nil})
	out, err := rs.safeWaterfall(ctx, lead, col, rowID, leadID)
	if err != nil {
		if a := abortErr(ctx, err); a != nil {
			return cellOutcome{}, a
		}
		rs.log.Error("cell crashed", "col", col.ID, "row", rowID, "err", err)
		rs.persistCellFailure(ctx, col, rowID, leadID)
		return cellOutcome{}, nil
	}
	return out, nil
}

// safeWaterfall runs the waterfall and turns a panic into an ordinary cell
// error, so one bad cell cannot take the process (and every other tenant's
// job) down. The stack is logged; payload values and secrets are not.
func (rs *runState) safeWaterfall(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (out cellOutcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			rs.log.Error("cell panicked", "col", col.ID, "row", rowID, "stack", string(debug.Stack()))
			out, err = cellOutcome{}, fmt.Errorf("panic: %v", r)
		}
	}()
	return rs.waterfall(ctx, lead, col, rowID, leadID)
}

// persistCellFailure ports _run_one_cell's except path.
func (rs *runState) persistCellFailure(ctx context.Context, col column, rowID, leadID int64) {
	msg := "cell_execution_failed"
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		if err := fence(ctx, tx, rs.claim, rs.p.WorkbookID); err != nil {
			return err
		}
		return setEnrichment(ctx, tx, cellWrite{WorkspaceID: rs.p.WorkspaceID, WorkbookID: rs.p.WorkbookID,
			LeadID: int32(leadID), RowID: int32(rowID), ColumnID: col.ID, Status: "error", Error: &msg})
	})
	if err != nil {
		rs.log.Warn("could not persist cell failure; ownership or storage may have changed", "err", err)
	}
}

func (rs *runState) waterfall(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (cellOutcome, error) {
	ws, wb := rs.p.WorkspaceID, rs.p.WorkbookID
	target := col.target()
	configured, explicit := col.explicitChain()
	chain := configured
	if !explicit {
		chain = append([]string(nil), defaultWaterfalls[target]...)
	}
	authorized := append([]string(nil), chain...)
	rowIdentity := fmt.Sprintf("row:%d", rowID)

	var resultValue, resultProvider, resultError string

	// ── cost-aware ordering + budget ceiling ──
	var budgetRemaining *float64
	if err := tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		b, e := gen.New(tx).GetWorkbookBudget(ctx, gen.GetWorkbookBudgetParams{WorkbookID: wb, WorkspaceID: ws})
		if e != nil {
			return e
		}
		if b.BudgetMaxUsd.Valid && b.BudgetMaxUsd.Float64 > 0 {
			r := b.BudgetMaxUsd.Float64 - b.BudgetSpentUsd.Float64
			budgetRemaining = &r
		}
		return nil
	}); err != nil {
		return cellOutcome{}, err
	}
	usable, skipped, err := rs.pl.plan(ctx, rs.deps.Pool, ws, target, chain, explicit, budgetRemaining)
	if err != nil {
		return cellOutcome{}, err
	}
	var chainExposure int64
	seen := map[string]bool{}
	for _, p := range authorized {
		if seen[p] {
			continue
		}
		seen[p] = true
		e, err := spend.ExposureFromUSD(rs.pl.cost(p))
		if err != nil {
			return cellOutcome{}, err
		}
		chainExposure += e
	}
	// Providers already attempted for this cell in this run go first.
	var prior []string
	if err := tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		var e error
		prior, e = gen.New(tx).ListPriorAttemptProviders(ctx, gen.ListPriorAttemptProvidersParams{
			WorkspaceID: ws, WorkbookID: wb, RunID: fmt.Sprintf("job:%d", rs.claim.ID), RowIdentity: rowIdentity, ColumnID: col.ID})
		return e
	}); err != nil {
		return cellOutcome{}, err
	}
	inAuth := map[string]bool{}
	for _, p := range authorized {
		inAuth[p] = true
	}
	merged, dup := []string{}, map[string]bool{}
	for _, p := range append(filterIn(prior, inAuth), usable...) {
		if !dup[p] {
			dup[p] = true
			merged = append(merged, p)
		}
	}
	chain = merged
	if mp := rs.p.MaxProviders; mp > 0 && len(chain) > mp {
		chain = chain[:mp]
	}

	attempted := 0 // provider_attempts entries of the Python cell
loop:
	for _, name := range chain {
		if err := checkOwner(ctx, rs.deps.Pool, rs.claim, wb); err != nil {
			return cellOutcome{}, err
		}
		if ctx.Err() != nil {
			return cellOutcome{}, context.Cause(ctx)
		}
		if _, declarative := rs.p.Manifests[name]; !declarative && rs.legacy {
			rs.log.Warn("provider not found, skipping", "provider", name)
			skipped = append(skipped, skippedProvider{name, "unknown_provider"})
			continue
		}
		reserved := rs.pl.paid(name)
		if !reserved && !rs.pl.knownCost(name) {
			resultError = "provider_price_unknown"
			break
		}
		var call providerCall
		if reserved {
			exposure, err := spend.ExposureFromUSD(rs.pl.cost(name))
			if err != nil {
				return cellOutcome{}, err
			}
			// Money moves only for calls that will really reach the network: a
			// missing key or a blocked URL is known before Reserve, so it never
			// creates an attempt or a budget hold (and never ends `uncertain`).
			if perr := rs.precheck(ctx, name, lead); perr != "" {
				call = providerCall{kind: callOK, resp: responseDict(name, providers.Output{Error: perr})}
			} else if call, err = rs.reservedCall(ctx, name, lead, col, target, rowIdentity, exposure, chainExposure); err != nil {
				return cellOutcome{}, err
			}
		} else {
			out, timedOut, rerr := rs.runProvider(ctx, name, lead, target)
			if ctx.Err() != nil {
				return cellOutcome{}, context.Cause(ctx)
			}
			switch {
			case errors.Is(rerr, pyworker.ErrUnknownProvider):
				call = providerCall{kind: callUnknown}
			case errors.Is(rerr, pyworker.ErrUnavailable), errors.Is(rerr, pyworker.ErrUnauthorized):
				return cellOutcome{}, fmt.Errorf("workbook: python worker: %w", rerr)
			case rerr != nil:
				call = providerCall{kind: callException, text: truncRunes(rerr.Error(), maxErrorRunes)}
			case timedOut:
				call = providerCall{kind: callTimeout}
			default:
				call = providerCall{kind: callOK, resp: responseDict(name, out)}
			}
		}

		// failed records a timeout/exception attempt; reserved calls stop the
		// waterfall as accounting_uncertain, free ones move to the next provider.
		stop := false
		failed := func(errText string) {
			resultError = errText
			attempted++
			if reserved {
				resultError = "accounting_uncertain"
				stop = true
			}
		}
		switch call.kind {
		case callReject, callInvalid:
			resultError = call.text
			break loop
		case callUnknown:
			rs.log.Warn("provider not found, skipping", "provider", name)
			skipped = append(skipped, skippedProvider{name, "unknown_provider"})
		case callTimeout:
			rs.log.Warn("provider timed out", "provider", name, "row", rowID)
			failed("timeout")
		case callException:
			rs.log.Error("provider failed", "provider", name, "row", rowID, "err", call.text)
			failed(call.text)
		case callOK:
			attempted++
			success, _ := call.resp["success"].(bool)
			fields, _ := call.resp["fields"].(map[string]any)
			if success && len(fields) > 0 {
				if v, ok := fields[target]; ok && manifest.PyTruthy(v) && v != "N/A" {
					val, perr := cellScalar(v)
					if perr != nil {
						failed(truncRunes(perr.Error(), maxErrorRunes))
					} else {
						resultValue, resultProvider = val, name
					}
				}
			}
			if resultValue != "" {
				break loop // waterfall: stop at first success
			}
		}
		if stop {
			break
		}
	}
	if resultValue == "" && resultError == "" && attempted == 0 {
		if len(authorized) == 0 {
			if explicit {
				resultError = "no_providers_selected"
			} else {
				resultError = "no_default_providers"
			}
		} else if len(skipped) > 0 {
			parts := make([]string, 0, len(skipped))
			for _, s := range skipped {
				parts = append(parts, fmt.Sprintf("%s (%s)", s.Provider, s.Reason))
			}
			resultError = truncRunes("providers_unavailable: "+strings.Join(parts, ", "), maxErrorRunes)
		}
	}
	if resultValue == "" && resultError == "contract_conflict" {
		// A paid cell this job already finished must never be overwritten by a
		// contract conflict: keep the stored value and log it.
		if v, ok, err := rs.completeCellValue(ctx, rowID, col.ID); err != nil {
			return cellOutcome{}, err
		} else if ok {
			rs.log.Warn("contract conflict on an already complete cell; keeping the stored value", "col", col.ID, "row", rowID)
			return cellOutcome{Success: true, Value: v}, nil
		}
	}
	return rs.commitCell(ctx, col, rowID, leadID, resultValue, resultProvider, resultError, skipped)
}

// completeCellValue returns the stored value of a complete overlay cell.
func (rs *runState) completeCellValue(ctx context.Context, rowID int64, colID string) (string, bool, error) {
	var val string
	var found bool
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).ListRowEnrichments(ctx, gen.ListRowEnrichmentsParams{
			WorkbookID: rs.p.WorkbookID, WorkspaceID: rs.p.WorkspaceID, RowIds: []int32{int32(rowID)}})
		if err != nil {
			return err
		}
		for _, r := range rows {
			enr, err := jsonObject(r.Enrichments)
			if err != nil {
				return err
			}
			if cell, ok := enr[colID].(map[string]any); ok && cell["status"] == "complete" {
				if s, ok := cell["value"].(string); ok && s != "" {
					val, found = s, true
				}
			}
		}
		return nil
	})
	return val, found, err
}

func filterIn(list []string, allowed map[string]bool) []string {
	var out []string
	for _, p := range list {
		if allowed[p] {
			out = append(out, p)
		}
	}
	return out
}

// cellScalar ports the scalar enforcement of the target value: JSON blobs
// become a short summary instead of being shown in a cell.
func cellScalar(v any) (string, error) {
	sv := manifest.PyStr(v)
	if !strings.HasPrefix(sv, "[{") && !strings.HasPrefix(sv, `{"`) {
		return sv, nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(sv), &parsed); err != nil {
		return truncRunes(sv, 80), nil
	}
	list, ok := parsed.([]any)
	if !ok || len(list) == 0 {
		return truncRunes(sv, 80), nil
	}
	first, ok := list[0].(map[string]any)
	if !ok {
		return "", errors.New("structured value entries must be objects")
	}
	name := ""
	for _, k := range []string{"name", "email"} {
		if x, present := first[k]; present && manifest.PyTruthy(canonify(x)) {
			name = manifest.PyStr(canonify(x))
			break
		}
	}
	if name == "" {
		return fmt.Sprintf("%d results", len(list)), nil
	}
	if len(list) > 1 {
		return fmt.Sprintf("%s +%d more", name, len(list)-1), nil
	}
	return name, nil
}

// canonify maps encoding/json decoded values onto the shapes PyStr understands.
func canonify(v any) any {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
	}
	return v
}

// commitCell is the "Write results" tail of enrich_cell: one lease-fenced
// transaction for the overlay row and the workbook row mirror, then the
// separate telemetry transaction, then the result broadcast.
func (rs *runState) commitCell(ctx context.Context, col column, rowID, leadID int64, value, provider, errText string,
	skipped []skippedProvider) (cellOutcome, error) {
	ws, wb := rs.p.WorkspaceID, rs.p.WorkbookID
	hasResult := value != ""
	w := cellWrite{WorkspaceID: ws, WorkbookID: wb, LeadID: int32(leadID), RowID: int32(rowID),
		ColumnID: col.ID, Skipped: skipped}
	var bErr any
	if hasResult {
		w.Status, w.Value, w.Provider = "complete", &value, &provider
	} else {
		stored := errText
		if stored == "" {
			stored = "no_data"
		}
		w.Status, w.Error = "error", &stored
		if errText != "" {
			bErr = errText // Python broadcasts result_error, which may be None
		}
	}
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		if err := fence(ctx, tx, rs.claim, wb); err != nil {
			return err
		}
		return setEnrichment(ctx, tx, w)
	})
	if err != nil {
		return cellOutcome{}, err
	}

	var bValue, bProvider any
	status := "error"
	if hasResult {
		bValue, bProvider, status = value, provider, "complete"
	}
	rs.publish(ctx, map[string]any{"type": "cell_update", "leadId": leadID, "rowId": rowID, "colId": col.ID,
		"value": bValue, "status": status, "provider": bProvider, "error": bErr,
		"research": nil, "provenance": nil, "verify_status": nil})
	return cellOutcome{Success: hasResult, Value: value}, nil
}

// upstreamFailed ports `any(referencing_columns([col], failed) ...)`.
func (rs *runState) upstreamFailed(col column, failed []column) bool {
	for _, f := range failed {
		if len(colgraph.Referencing([]colgraph.Col{col.g}, f.g)) > 0 {
			return true
		}
	}
	return false
}

// dependencyError ports the dependency_error block of enrich_cell: cycle,
// then unavailable upstream value, then (overriding) ambiguous references.
func (rs *runState) dependencyError(lead map[string]any, col column) string {
	derr := ""
	if rs.cycleBlocked[col.ID] {
		derr = "dependency_cycle"
	} else if len(colgraph.UnavailableDeps(col.g, rs.allCols, lead)) > 0 {
		derr = "upstream_dependency_unavailable"
	}
	for _, ref := range colgraph.Refs(col.g) {
		if len(colgraph.ReferenceIndexes(manifest.PyStrip(ref), rs.allCols)) > 1 {
			derr = "ambiguous_column_reference"
			break
		}
	}
	return derr
}

// failDependent persists an error cell without calling any provider. Python
// writes it fenced and does not broadcast it.
func (rs *runState) failDependent(ctx context.Context, lead map[string]any, col column, msg string) (cellOutcome, error) {
	rowID, leadID := intOf(lead["__row_id"]), intOf(lead["id"])
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		if err := fence(ctx, tx, rs.claim, rs.p.WorkbookID); err != nil {
			return err
		}
		return setEnrichment(ctx, tx, cellWrite{WorkspaceID: rs.p.WorkspaceID, WorkbookID: rs.p.WorkbookID,
			LeadID: int32(leadID), RowID: int32(rowID), ColumnID: col.ID, Status: "error", Error: &msg})
	})
	if err != nil {
		if a := abortErr(ctx, err); a != nil {
			return cellOutcome{}, a
		}
		return cellOutcome{}, err
	}
	return cellOutcome{}, nil
}
