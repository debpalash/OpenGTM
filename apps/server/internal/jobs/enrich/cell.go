package enrich

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/template"
	"github.com/debpalash/OpenGTM/apps/server/internal/progress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// cellOutcome is enrich_cell's {success, value, provider, error}.
type cellOutcome struct {
	Success  bool
	Value    string // meaningful when Success
	Provider string
	Error    string
}

type skippedProvider struct{ provider, reason string }

// attemptRecord is one provider_attempts entry, flushed to provider_stats
// after the cell commits.
type attemptRecord struct {
	provider, field string
	success         bool
	confidence      float64
	latencyMS       float64
	timedOut        bool
	rateLimited     bool
}

const bgTimeout = 30 * time.Second

// detached keeps bookkeeping that must complete (settling a paid answer,
// marking an attempt uncertain) alive when the attempt's context is cancelled.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bgTimeout)
}

func truncate(s string, n int) string { return template.Truncate(s, n) }

// checkOwner is check_workbook_run_owner: a preflight, with the lock released
// before any external call.
func (r *run) checkOwner(ctx context.Context) error {
	return db.WithoutTenant(ctx, r.w.pool, func(tx pgx.Tx) error { return queue.HoldLease(ctx, tx, r.job) })
}

// cooldowns lists the providers benched for field (planner.in_cooldown). The
// stored naive timestamp is compared as UTC, exactly as Python reads it.
func (r *run) cooldowns(ctx context.Context, field string, chain []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(chain) == 0 {
		return out, nil
	}
	rows, err := r.w.pool.Query(ctx, `SELECT provider FROM provider_stats
		WHERE field = $1 AND provider = ANY($2::text[]) AND cooldown_until > (now() AT TIME ZONE 'UTC')`, field, chain)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

// leadIDOf is lead_data["id"] (the legacy integer cell key).
func leadIDOf(lead *pycompat.Map) int64 {
	switch v := mapGet(lead, "id").(type) {
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// runCell is enrich_cell for a waterfall/enrichment column over a row that has
// no linked lead. A returned error means the attempt must stop (lease lost or
// cancelled); everything a provider can do wrong is an outcome, not an error.
func (r *run) runCell(ctx context.Context, rowID int64, lead *pycompat.Map, col *column) (cellOutcome, error) {
	out, err := r.runCellInner(ctx, rowID, lead, col)
	if err == nil || errors.Is(err, queue.ErrLeaseLost) || ctx.Err() != nil {
		return out, err
	}
	// _run_one_cell: an unexpected failure becomes a persisted cell error.
	r.w.log.Error("cell failed", "job_id", r.job.ID, "column", col.id, "row", rowID, "err", err)
	if perr := r.persistCellFailure(ctx, rowID, col); perr != nil && (errors.Is(perr, queue.ErrLeaseLost) || ctx.Err() != nil) {
		return cellOutcome{}, perr
	}
	return cellOutcome{Error: truncate(err.Error(), 200)}, nil
}

func (r *run) persistCellFailure(ctx context.Context, rowID int64, col *column) error {
	return db.WithTenant(ctx, r.w.pool, r.ws, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, r.job); err != nil {
			return err
		}
		return writeCell(ctx, tx, r.ws, r.wbID, rowID, rowID, col.id, cellWrite{status: "error", err: "cell_execution_failed"})
	})
}

func (r *run) runCellInner(ctx context.Context, rowID int64, lead *pycompat.Map, col *column) (out cellOutcome, err error) {
	ws, wb := r.ws, r.wbID
	leadID := leadIDOf(lead)
	rowIdentity := fmt.Sprintf("row:%d", rowID)
	target := col.targetField()
	authorized, _ := col.explicitChain()
	chain := slices.Clone(authorized)

	// Prologue: lease check, the workbook budget and this cell's earlier
	// attempts, in one short transaction (no network I/O inside).
	var budgetRemaining *float64
	var prior []string
	err = db.WithTenant(ctx, r.w.pool, ws, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, r.job); err != nil {
			return err
		}
		var capUSD, spentUSD *float64
		switch e := tx.QueryRow(ctx, `SELECT budget_max_usd, budget_spent_usd FROM workbooks WHERE id = $1`, wb).
			Scan(&capUSD, &spentUSD); {
		case errors.Is(e, pgx.ErrNoRows):
		case e != nil:
			return e
		}
		maxUSD, spent := 0.0, 0.0
		if capUSD != nil {
			maxUSD = *capUSD
		}
		if spentUSD != nil {
			spent = *spentUSD
		}
		if maxUSD > 0 {
			rem := maxUSD - spent
			budgetRemaining = &rem
		}
		var e error
		prior, e = priorProviders(ctx, tx, ws, wb, r.runID(), rowIdentity, col.id)
		return e
	})
	if err != nil {
		return out, err
	}

	// planner.filter_chain: cooldowns and the budget ceiling, user order kept.
	var skipped []skippedProvider
	cool, err := r.cooldowns(ctx, target, chain)
	if err != nil {
		return out, err
	}
	usable := make([]string, 0, len(chain))
	for _, name := range chain {
		if cool[name] {
			skipped = append(skipped, skippedProvider{name, "cooldown"})
			continue
		}
		p := r.w.plugin(name)
		if budgetRemaining != nil && isPaid(name, p) && baseCost(name, p) > *budgetRemaining {
			skipped = append(skipped, skippedProvider{name, "over_budget"})
			continue
		}
		usable = append(usable, name)
	}
	chain = usable

	var chainExposure int64
	seen := map[string]bool{}
	for _, name := range authorized {
		if !seen[name] {
			seen[name] = true
			chainExposure += microUSD(baseCost(name, r.w.plugin(name)))
		}
	}
	// A retry resumes with the providers it already reserved, in order.
	var resumed []string
	for _, name := range prior {
		if slices.Contains(authorized, name) {
			resumed = append(resumed, name)
		}
	}
	chain = dedupe(append(resumed, chain...))
	if r.pl.MaxProviders > 0 && len(chain) > r.pl.MaxProviders {
		chain = chain[:r.pl.MaxProviders]
	}

	cr := &cellRun{r: r}
	defer func() {
		// Whatever ends the cell, a paid answer that arrived is recorded.
		if cr.pending != nil {
			if ferr := cr.flush(ctx); ferr != nil && err == nil {
				err = ferr
			}
		}
	}()

	var (
		value          string
		valueProvider  string
		valueEvidence  *declarative.ProviderResult
		valueConf      float64
		resultError    string
		attempts       []attemptRecord
		evidenceSource *declarative.ProviderEvidence
	)

	for _, name := range chain {
		if cr.pending != nil {
			if ferr := cr.flush(ctx); ferr != nil {
				resultError = "accounting_uncertain"
				break
			}
		}
		if err := r.checkOwner(ctx); err != nil {
			return out, err
		}
		plugin := r.w.plugin(name)
		if plugin == nil {
			skipped = append(skipped, skippedProvider{name, "unknown_provider"})
			continue
		}
		t0 := time.Now()
		reserved := isPaid(name, plugin)
		if !reserved && !hasKnownCost(name, plugin) {
			resultError = "provider_price_unknown"
			break
		}

		var (
			resp   response
			evid   *declarative.ProviderResult
			callOK bool
			pend   *settlement
		)
		fail := func(msg string, timedOut bool) (stop bool) {
			// The `except` arms of the provider loop.
			rec := attemptRecord{provider: name, field: target, latencyMS: float64(time.Since(t0).Microseconds()) / 1000}
			if timedOut {
				rec.timedOut = true
				resultError = "timeout"
			} else {
				resultError = truncate(msg, 200)
				rec.rateLimited = looksRateLimited(resultError)
			}
			attempts = append(attempts, rec)
			if reserved {
				resultError = "accounting_uncertain"
				return true
			}
			return false
		}

		if reserved {
			exposure := microUSD(baseCost(name, plugin))
			key, kerr := attemptKey(ws, wb, r.runID(), rowIdentity, col.id, name)
			if kerr != nil {
				if fail(kerr.Error(), false) {
					break
				}
				continue
			}
			rsv, rerr := r.led.reserve(ctx, reservation{
				workspaceID: ws, workbookID: wb, runID: r.runID(), rowIdentity: rowIdentity, columnID: col.id,
				provider: name, attemptKey: key, exposure: exposure, cellLimit: chainExposure,
				costBasis: obj("kind", "catalog_estimate", "provider", name),
				operation: obj("inputs", lead, "column", col.cfg),
			})
			if rerr != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				if fail(rerr.Error(), false) {
					break
				}
				continue
			}
			if !rsv.OK {
				resultError = rsv.Reason
				break
			}
			if rsv.Status == "settled" {
				m, _, _, found, serr := r.led.settledReceipt(ctx, ws, wb, rsv.ID, rsv.Hash)
				if serr != nil || !found {
					if ctx.Err() != nil {
						return out, ctx.Err()
					}
					if fail("settled attempt has no receipt", false) {
						break
					}
					continue
				}
				inner, _ := mapGet(m, "result").(*pycompat.Map)
				if inner == nil {
					inner = pycompat.NewMap()
				}
				resp, callOK = responseFromMap(inner), true
			} else {
				ok, derr := r.led.dispatch(ctx, ws, wb, rsv.ID, rsv.Hash)
				if derr != nil && ctx.Err() != nil {
					return out, ctx.Err()
				}
				if derr != nil {
					if fail(derr.Error(), false) {
						break
					}
					continue
				}
				if !ok {
					resultError = "attempt_not_dispatchable"
					break
				}
				var cerr error
				resp, evid, cerr = r.caller.call(ctx, name, plugin, lead)
				if cerr == nil {
					charged, basis, eerr := accountingEnvelope(name, resp, exposure)
					if eerr != nil {
						cerr = eerr
					} else {
						pend = &settlement{
							workspaceID: ws, workbookID: wb, attemptID: rsv.ID, contractHash: rsv.Hash,
							charged: charged, basis: basis, receipt: receiptFor(basis, resp),
						}
					}
				}
				if cerr != nil {
					// The provider may have been reached: never retryable again.
					bctx, cancel := detached(ctx)
					merr := r.led.markUncertain(bctx, ws, wb, rsv.ID, rsv.Hash)
					cancel()
					if merr != nil {
						r.w.log.Warn("could not mark attempt uncertain", "attempt", rsv.ID, "err", merr)
					}
					if ctx.Err() != nil {
						return out, ctx.Err()
					}
					if fail(cerr.Error(), errors.Is(cerr, errTimeout)) {
						break
					}
					continue
				}
				callOK = true
			}
		} else {
			var cerr error
			resp, evid, cerr = r.caller.call(ctx, name, plugin, lead)
			if cerr != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				if fail(cerr.Error(), errors.Is(cerr, errTimeout)) {
					break
				}
				continue
			}
			callOK = true
		}
		if !callOK {
			continue
		}
		cr.pending = pend

		conf := resp.Confidence
		if conf == 0 && plugin.Provider != nil {
			conf = plugin.Provider.DefaultConfidence
		}
		hasFields := resp.Fields != nil && resp.Fields.Len() > 0
		attempts = append(attempts, attemptRecord{
			provider: name, field: target, success: resp.Success && hasFields,
			confidence: conf, latencyMS: float64(time.Since(t0).Microseconds()) / 1000,
		})
		if resp.Success && hasFields {
			if v, found := resp.Fields.Get(target); found && pycompat.Truthy(v) && v != "N/A" {
				cell, cerr := cellText(v)
				if cerr != nil {
					// An exception inside the provider's try block.
					resultError = truncate(cerr.Error(), 200)
					attempts = append(attempts, attemptRecord{
						provider: name, field: target, latencyMS: float64(time.Since(t0).Microseconds()) / 1000,
						rateLimited: looksRateLimited(resultError),
					})
					if reserved {
						resultError = "accounting_uncertain"
						break
					}
					continue
				}
				value, valueProvider, valueConf, valueEvidence = cell, name, conf, evid
			}
		}
		if value != "" {
			break // waterfall: stop at the first success
		}
	}
	if valueEvidence != nil {
		evidenceSource = valueEvidence.Evidence
	}

	if value == "" && resultError == "" && len(attempts) == 0 {
		switch {
		case len(authorized) == 0:
			resultError = "no_providers_selected"
		case len(skipped) > 0:
			parts := make([]string, len(skipped))
			for i, s := range skipped {
				parts[i] = fmt.Sprintf("%s (%s)", s.provider, s.reason)
			}
			resultError = truncate("providers_unavailable: "+joinReasons(parts), 200)
		}
	}

	w := cellWrite{skipped: skipped}
	if value != "" {
		w.status, w.value, w.provider = "complete", &value, valueProvider
		w.evidence = evidenceMetadata(valueProvider, valueConf, evidenceSource, r.w.plugin(valueProvider))
	} else {
		w.status, w.err = "error", resultError
		if w.err == "" {
			w.err = "no_data"
		}
	}
	_ = valueConf

	if err := r.commitCell(ctx, cr, rowID, leadID, col.id, w); err != nil {
		return out, err
	}
	r.recordAttempts(ctx, attempts)

	if value != "" {
		return cellOutcome{Success: true, Value: value, Provider: valueProvider}, nil
	}
	return cellOutcome{Error: resultError}, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// obj builds an ordered pycompat dictionary from alternating keys and values.
func obj(kv ...any) *pycompat.Map {
	m := pycompat.NewMap()
	for i := 0; i+1 < len(kv); i += 2 {
		_ = m.Set(kv[i], kv[i+1])
	}
	return m
}

// cellText is the scalar rule of enrich_cell: JSON blobs never land in a
// cell; they are summarised.
func cellText(v any) (string, error) {
	sv := pycompat.Str(v)
	if !strings.HasPrefix(sv, "[{") && !strings.HasPrefix(sv, `{"`) {
		return sv, nil
	}
	parsed, err := pycompat.LoadJSON([]byte(sv))
	if err != nil {
		return truncate(sv, 80), nil
	}
	list, ok := parsed.([]any)
	if !ok || len(list) == 0 {
		return truncate(sv, 80), nil
	}
	first, ok := list[0].(*pycompat.Map)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'get'", pycompat.TypeName(list[0]))
	}
	name := mapGet(first, "name")
	if !pycompat.Truthy(name) {
		name = mapGet(first, "email")
	}
	if pycompat.Truthy(name) {
		s := pycompat.Str(name)
		if len(list) > 1 {
			s += fmt.Sprintf(" +%d more", len(list)-1)
		}
		return s, nil
	}
	return fmt.Sprintf("%d results", len(list)), nil
}

// cellRun carries the settlement of the cell's most recent paid call, which is
// committed with the cell's result.
type cellRun struct {
	r       *run
	pending *settlement
}

// flush settles the pending paid attempt in its own transaction (before the
// next provider call, or when the cell ends without reaching its commit).
func (c *cellRun) flush(ctx context.Context) error {
	s := c.pending
	c.pending = nil
	if s == nil {
		return nil
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	res, err := c.r.led.settleAlone(bctx, *s)
	if err == nil && res.OK {
		return nil
	}
	if merr := c.r.led.markUncertain(bctx, s.workspaceID, s.workbookID, s.attemptID, s.contractHash); merr != nil {
		c.r.w.log.Warn("could not mark attempt uncertain", "attempt", s.attemptID, "err", merr)
	}
	if err == nil {
		err = fmt.Errorf("Provider outcome settlement was not confirmed: %s", res.Reason)
	}
	return err
}

// cellWrite is what a finished cell persists.
type cellWrite struct {
	status   string
	value    *string
	provider string
	err      string
	skipped  []skippedProvider
	evidence *pycompat.Map
}

// commitCell is the cell's result transaction: under the job lease it settles
// the last paid attempt (budget included), upserts the overlay row, mirrors
// the cell into the row JSON, and publishes progress. If the lease is gone the
// answer is still accounted, but no result is written.
func (r *run) commitCell(ctx context.Context, cr *cellRun, rowID, leadID int64, colID string, w cellWrite) error {
	var settleFailed bool
	err := db.WithTenant(ctx, r.w.pool, r.ws, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, r.job); err != nil {
			return err
		}
		if cr.pending != nil {
			res, err := settleTx(ctx, tx, *cr.pending)
			if err != nil {
				return err
			}
			if !res.OK {
				settleFailed = true
				return errSettlement
			}
		}
		if err := writeCell(ctx, tx, r.ws, r.wbID, rowID, leadID, colID, w); err != nil {
			return err
		}
		return progress.Publish(ctx, tx, r.ws, "workbook_cell_update", cellEvent(r.wbID, rowID, colID, w))
	})
	switch {
	case err == nil:
		cr.pending = nil
		return nil
	case settleFailed:
		// Settlement was refused: the attempt cannot be trusted either way.
		ferr := cr.flush(ctx)
		_ = ferr
		return r.commitFailure(ctx, rowID, leadID, colID, "accounting_uncertain")
	case errors.Is(err, queue.ErrLeaseLost):
		_ = cr.flush(ctx)
		return err
	}
	// Any other failure rolled the whole transaction back, settlement included.
	_ = cr.flush(ctx)
	return err
}

var errSettlement = errors.New("settlement not confirmed")

func (r *run) commitFailure(ctx context.Context, rowID, leadID int64, colID, reason string) error {
	return db.WithTenant(ctx, r.w.pool, r.ws, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, r.job); err != nil {
			return err
		}
		return writeCell(ctx, tx, r.ws, r.wbID, rowID, leadID, colID, cellWrite{status: "error", err: reason})
	})
}

func cellEvent(wb string, rowID int64, colID string, w cellWrite) map[string]any {
	ev := map[string]any{"type": "cell_update", "workbook_id": wb, "row_id": rowID, "col_id": colID, "status": w.status}
	if w.value != nil {
		ev["value"] = truncate(*w.value, 500)
		ev["provider"] = w.provider
	} else {
		ev["error"] = w.err
	}
	return ev
}

// evidenceMetadata describes where a written value came from. It is an
// additive key (cell_metadata.evidence) that the Python path does not write:
// the source URL (secrets redacted), the response status, size and digest, and
// the field mappings that produced the value.
func evidenceMetadata(provider string, confidence float64, ev *declarative.ProviderEvidence, p *manifest.Plugin) *pycompat.Map {
	if ev == nil {
		return nil
	}
	m := obj("provider", provider, "source_url", ev.SourceURL, "method", ev.Method, "status", int64(ev.Status),
		"fetched_at", ev.FetchedAt.UTC().Format(time.RFC3339Nano), "bytes", int64(ev.Bytes), "sha256", ev.SHA256,
		"confidence", confidence)
	if ev.ContentType != "" {
		_ = m.Set("content_type", ev.ContentType)
	}
	maps := pycompat.NewMap()
	for k, v := range ev.Mappings {
		_ = maps.Set(k, v)
	}
	_ = m.Set("mappings", maps)
	if p != nil && p.Provider != nil {
		_ = m.Set("cost_usd", p.Provider.CostPerLookup)
	}
	return m
}

// recordAttempts is planner.record_attempt for every attempt of the cell:
// reliability telemetry written after the result commit, never part of it.
func (r *run) recordAttempts(ctx context.Context, attempts []attemptRecord) {
	if len(attempts) == 0 {
		return
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	for _, a := range attempts {
		if err := r.w.recordAttempt(bctx, a); err != nil {
			r.w.log.Warn("provider telemetry write failed", "provider", a.provider, "err", err)
			return
		}
	}
}
