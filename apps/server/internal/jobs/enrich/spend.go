package enrich

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// The spend ledger is a port of apps/api/services/workbook/spend_service.py and
// provider_accounting.py: the same table, the same state machine (reserved ->
// dispatched -> settled | uncertain, reserved -> released), the same contract
// digest and attempt key, so an attempt reserved by one executor can be
// continued by the other after a routing change.

type reservation struct {
	workspaceID, workbookID, runID, rowIdentity, columnID, provider, attemptKey string
	exposure, cellLimit                                                         int64
	costBasis, operation                                                        *pycompat.Map
}

type reserveOutcome struct {
	OK     bool
	Reason string
	ID     string
	Status string
	Hash   string
}

type ledger struct {
	pool *pgxpool.Pool
	job  queue.Job
}

func epochNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func ratFromFloat(f float64) *big.Rat {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	if !ok {
		return new(big.Rat)
	}
	return r
}

// contractJSON is json.dumps(contract, sort_keys=True, separators=(",", ":"),
// allow_nan=False).
func contractJSON(r reservation) ([]byte, error) {
	c := pycompat.NewMap()
	_ = c.Set("workbook_id", r.workbookID)
	_ = c.Set("run_id", r.runID)
	_ = c.Set("row_identity", r.rowIdentity)
	_ = c.Set("column_id", r.columnID)
	_ = c.Set("provider", r.provider)
	_ = c.Set("exposure", r.exposure)
	_ = c.Set("cell_limit", r.cellLimit)
	_ = c.Set("cost_basis", r.costBasis)
	_ = c.Set("operation", r.operation)
	return pycompat.Dumps(c, pycompat.DumpOptions{SortKeys: true, EnsureASCII: true, ItemSep: ",", KeySep: ":", DisallowNaN: true})
}

func identityOK(s string) bool {
	n := utf8.RuneCountInString(s)
	return n > 0 && n <= 255
}

// reserve is spend_service.reserve_attempt. It reserves catalog exposure; it
// does not authorize a dispatch.
func (l *ledger) reserve(ctx context.Context, r reservation) (reserveOutcome, error) {
	for _, v := range []string{r.workspaceID, r.workbookID, r.runID, r.rowIdentity, r.columnID, r.provider, r.attemptKey} {
		if !identityOK(v) {
			return reserveOutcome{}, errors.New("Bounded scoped identities are required")
		}
	}
	if r.exposure < 0 || r.cellLimit < 0 {
		return reserveOutcome{}, errors.New("Exposure and cell limit must be nonnegative integer micro-USD")
	}
	if r.costBasis.Len() == 0 {
		return reserveOutcome{}, errors.New("An explicit cost basis is required")
	}
	if r.operation.Len() == 0 {
		return reserveOutcome{}, errors.New("An explicit provider operation contract is required")
	}
	serialized, err := contractJSON(r)
	if err != nil {
		return reserveOutcome{}, err
	}
	sum := sha256.Sum256(serialized)
	digest := hex.EncodeToString(sum[:])
	basisJSON, err := pycompat.Dumps(r.costBasis, pycompat.DumpOptions{EnsureASCII: true})
	if err != nil {
		return reserveOutcome{}, err
	}

	var out reserveOutcome
	err = db.WithTenant(ctx, l.pool, r.workspaceID, func(tx pgx.Tx) error {
		// All admissions serialize on the workbook row.
		tag, err := tx.Exec(ctx, `UPDATE workbooks SET budget_spent_usd = budget_spent_usd
			WHERE id = $1 AND workspace_id = $2`, r.workbookID, r.workspaceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			out = reserveOutcome{Reason: "workbook_not_found"}
			return nil
		}
		var prevID, prevHash, prevStatus string
		err = tx.QueryRow(ctx, `SELECT id, contract_hash, status FROM workbook_spend_attempts
			WHERE workspace_id = $1 AND attempt_key = $2`, r.workspaceID, r.attemptKey).Scan(&prevID, &prevHash, &prevStatus)
		switch {
		case err == nil:
			if prevHash != digest {
				out = reserveOutcome{Reason: "contract_conflict"}
				return nil
			}
			out = reserveOutcome{OK: true, ID: prevID, Status: prevStatus, Hash: digest}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var capUSD, spentUSD *float64
		if err := tx.QueryRow(ctx, `SELECT budget_max_usd, budget_spent_usd FROM workbooks WHERE id = $1`,
			r.workbookID).Scan(&capUSD, &spentUSD); err != nil {
			return err
		}
		var outstanding, cellExposure int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(reserved_microusd), 0)::bigint FROM workbook_spend_attempts
			WHERE workspace_id = $1 AND workbook_id = $2 AND status IN ('reserved', 'dispatched', 'uncertain')`,
			r.workspaceID, r.workbookID).Scan(&outstanding); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(coalesce(settled_microusd, reserved_microusd)), 0)::bigint
			FROM workbook_spend_attempts
			WHERE workspace_id = $1 AND workbook_id = $2 AND run_id = $3 AND row_identity = $4
			  AND column_id = $5 AND status != 'released'`,
			r.workspaceID, r.workbookID, r.runID, r.rowIdentity, r.columnID).Scan(&cellExposure); err != nil {
			return err
		}
		if cellExposure+r.exposure > r.cellLimit {
			out = reserveOutcome{Reason: "cell_budget"}
			return nil
		}
		capV, spentV := 0.0, 0.0
		if capUSD != nil {
			capV = *capUSD
		}
		if spentUSD != nil {
			spentV = *spentUSD
		}
		if math.IsNaN(capV) || math.IsInf(capV, 0) || math.IsNaN(spentV) || math.IsInf(spentV, 0) || capV < 0 || spentV < 0 {
			out = reserveOutcome{Reason: "invalid_workbook_budget"}
			return nil
		}
		if capV > 0 {
			spentMicro := new(big.Rat).Mul(ratFromFloat(spentV), big.NewRat(1000000, 1))
			capMicro := new(big.Rat).Mul(ratFromFloat(capV), big.NewRat(1000000, 1))
			if ratCeil(spentMicro)+outstanding+r.exposure > ratFloor(capMicro) {
				out = reserveOutcome{Reason: "workbook_budget"}
				return nil
			}
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO workbook_spend_attempts
			(id, workspace_id, workbook_id, run_id, row_identity, column_id, provider, attempt_key, contract_hash,
			 reserved_microusd, cost_basis, status, created_at, updated_at)
			VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10::json, 'reserved', $11, $11)
			RETURNING id`,
			r.workspaceID, r.workbookID, r.runID, r.rowIdentity, r.columnID, r.provider, r.attemptKey, digest,
			r.exposure, string(basisJSON), epochNow()).Scan(&id); err != nil {
			return err
		}
		out = reserveOutcome{OK: true, ID: id, Status: "reserved", Hash: digest}
		return nil
	})
	return out, err
}

// dispatch is transition_attempt(action="dispatch"): reserved -> dispatched,
// only for the run of this job and only while the job lease is held. A false
// result never authorizes a provider call.
func (l *ledger) dispatch(ctx context.Context, ws, wb, attemptID, hash string) (bool, error) {
	var changed bool
	err := db.WithTenant(ctx, l.pool, ws, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, l.job); err != nil {
			if errors.Is(err, queue.ErrLeaseLost) {
				return nil
			}
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE workbook_spend_attempts SET status = 'dispatched', updated_at = $6
			WHERE id = $1 AND workspace_id = $2 AND workbook_id = $3 AND contract_hash = $4
			  AND status = 'reserved' AND run_id = $5`,
			attemptID, ws, wb, hash, fmt.Sprintf("job:%d", l.job.ID), epochNow())
		changed = err == nil && tag.RowsAffected() == 1
		return err
	})
	return changed, err
}

// markUncertain is transition_attempt(action="mark_uncertain"). It is not
// lease fenced: an attempt that may have reached the vendor must stop being
// retryable even when this worker lost its lease.
func (l *ledger) markUncertain(ctx context.Context, ws, wb, attemptID, hash string) error {
	return db.WithTenant(ctx, l.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE workbook_spend_attempts SET status = 'uncertain', updated_at = $5
			WHERE id = $1 AND workspace_id = $2 AND workbook_id = $3 AND contract_hash = $4 AND status = 'dispatched'`,
			attemptID, ws, wb, hash, epochNow())
		return err
	})
}

// settlement is a known provider outcome waiting to be accounted.
type settlement struct {
	workspaceID, workbookID, attemptID, contractHash string
	charged                                          int64
	basis                                            string
	receipt                                          *pycompat.Map // {"accounting_basis", "result"}
}

type settleResult struct {
	OK     bool
	Reason string
	Reused bool
}

// settleTx is spend_service.settle_attempt inside the caller's transaction:
// record the outcome and advance budget_spent_usd exactly once. Overruns are
// recorded, never clipped to the reservation.
func settleTx(ctx context.Context, tx pgx.Tx, s settlement) (settleResult, error) {
	tag, err := tx.Exec(ctx, `UPDATE workbooks SET budget_spent_usd = budget_spent_usd
		WHERE id = $1 AND workspace_id = $2`, s.workbookID, s.workspaceID)
	if err != nil {
		return settleResult{}, err
	}
	if tag.RowsAffected() != 1 {
		return settleResult{Reason: "workbook_not_found"}, nil
	}
	var status string
	var settled *int64
	var result *string
	err = tx.QueryRow(ctx, `SELECT status, settled_microusd, result::text FROM workbook_spend_attempts
		WHERE id = $1 AND workspace_id = $2 AND workbook_id = $3 AND contract_hash = $4 FOR UPDATE`,
		s.attemptID, s.workspaceID, s.workbookID, s.contractHash).Scan(&status, &settled, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return settleResult{Reason: "attempt_not_found"}, nil
	}
	if err != nil {
		return settleResult{}, err
	}
	receiptJSON, err := pycompat.Dumps(s.receipt, pycompat.DumpOptions{EnsureASCII: true, DisallowNaN: true})
	if err != nil {
		return settleResult{}, err
	}
	if status == "settled" {
		if settled == nil || *settled != s.charged || result == nil || !sameJSON(*result, string(receiptJSON)) {
			return settleResult{Reason: "settlement_conflict"}, nil
		}
		return settleResult{OK: true, Reused: true}, nil
	}
	if status != "dispatched" {
		return settleResult{Reason: "invalid_state"}, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE workbook_spend_attempts
		SET status = 'settled', settled_microusd = $2, result = $3::json, updated_at = $4 WHERE id = $1`,
		s.attemptID, s.charged, string(receiptJSON), epochNow()); err != nil {
		return settleResult{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workbooks
		SET budget_spent_usd = coalesce(budget_spent_usd, 0) + $3 WHERE id = $1 AND workspace_id = $2`,
		s.workbookID, s.workspaceID, float64(s.charged)/1000000); err != nil {
		return settleResult{}, err
	}
	return settleResult{OK: true}, nil
}

// settleAlone settles in its own tenant transaction (not lease fenced: a paid
// answer that arrived must be recorded even when the lease was lost).
func (l *ledger) settleAlone(ctx context.Context, s settlement) (settleResult, error) {
	var res settleResult
	err := db.WithTenant(ctx, l.pool, s.workspaceID, func(tx pgx.Tx) error {
		var err error
		res, err = settleTx(ctx, tx, s)
		return err
	})
	return res, err
}

func sameJSON(a, b string) bool {
	av, err1 := pycompat.LoadJSON([]byte(a))
	bv, err2 := pycompat.LoadJSON([]byte(b))
	if err1 != nil || err2 != nil {
		return false
	}
	ac, e1 := pycompat.Canonical(av)
	bc, e2 := pycompat.Canonical(bv)
	return e1 == nil && e2 == nil && string(ac) == string(bc)
}

// settledReceipt loads the stored outcome of an already settled attempt.
func (l *ledger) settledReceipt(ctx context.Context, ws, wb, attemptID, hash string) (*pycompat.Map, int64, string, bool, error) {
	var raw string
	var charged int64
	err := db.WithTenant(ctx, l.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT result::text, settled_microusd FROM workbook_spend_attempts
			WHERE id = $1 AND workspace_id = $2 AND workbook_id = $3 AND contract_hash = $4 AND status = 'settled'`,
			attemptID, ws, wb, hash).Scan(&raw, &charged)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, "", false, nil
	}
	if err != nil {
		return nil, 0, "", false, err
	}
	v, err := pycompat.LoadJSON([]byte(raw))
	if err != nil {
		return nil, 0, "", false, err
	}
	m, _ := v.(*pycompat.Map)
	if m == nil {
		return nil, 0, "", false, errors.New("settled attempt has no receipt")
	}
	basis, _ := mapGet(m, "accounting_basis").(string)
	return m, charged, basis, true, nil
}

// priorProviders lists providers already attempted for this cell in this run,
// oldest first, so a retry resumes where it stopped.
func priorProviders(ctx context.Context, tx pgx.Tx, ws, wb, runID, rowIdentity, columnID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT provider FROM workbook_spend_attempts
		WHERE workspace_id = $1 AND workbook_id = $2 AND run_id = $3 AND row_identity = $4 AND column_id = $5
		ORDER BY created_at, id`, ws, wb, runID, rowIdentity, columnID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// attemptKey is WorkbookExecutionIdentity.attempt_key: sha256 of the compact
// ASCII-escaped JSON array [workspace, workbook, run, row, column, provider].
func attemptKey(ws, wb, runID, rowIdentity, columnID, provider string) (string, error) {
	for _, v := range []string{rowIdentity, columnID, provider} {
		if v == "" {
			return "", errors.New("Explicit row, column and provider identities required")
		}
	}
	parts := []any{ws, wb, runID, rowIdentity, columnID, provider}
	b, err := pycompat.Dumps(parts, pycompat.DumpOptions{EnsureASCII: true, ItemSep: ","})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// envelope is provider_accounting.accounting_envelope for a response that
// carries no vendor billing evidence (connectors never do).
type uncertainAccounting struct{ msg string }

func (e *uncertainAccounting) Error() string { return e.msg }

func accountingEnvelope(provider string, resp response, estimated int64) (int64, string, error) {
	if estimated < 0 {
		return 0, "", errors.New("Invalid catalog exposure")
	}
	if resp.Provider != provider {
		return 0, "", &uncertainAccounting{"Missing or mismatched provider result"}
	}
	if estimated == 0 || resp.Success {
		return estimated, "catalog_estimate", nil
	}
	return 0, "", &uncertainAccounting{"Paid unsuccessful lookup has no billing evidence"}
}

// response is the provider result dictionary provider_runner._provider_job
// returns, in its key order.
type response struct {
	Provider   string
	Success    bool
	Fields     *pycompat.Map
	Confidence float64
	Error      string
	License    string
}

func (r response) toMap() *pycompat.Map {
	m := pycompat.NewMap()
	fields := r.Fields
	if fields == nil {
		fields = pycompat.NewMap()
	}
	_ = m.Set("provider", r.Provider)
	_ = m.Set("success", r.Success)
	_ = m.Set("fields", fields)
	_ = m.Set("confidence", r.Confidence)
	_ = m.Set("error", r.Error)
	_ = m.Set("billing_evidence", nil)
	_ = m.Set("license", r.License)
	return m
}

func responseFromMap(m *pycompat.Map) response {
	r := response{}
	r.Provider, _ = mapGet(m, "provider").(string)
	r.Success, _ = mapGet(m, "success").(bool)
	r.Fields, _ = mapGet(m, "fields").(*pycompat.Map)
	switch c := mapGet(m, "confidence").(type) {
	case float64:
		r.Confidence = c
	case int64:
		r.Confidence = float64(c)
	}
	r.Error, _ = mapGet(m, "error").(string)
	r.License, _ = mapGet(m, "license").(string)
	return r
}

func receiptFor(basis string, resp response) *pycompat.Map {
	m := pycompat.NewMap()
	_ = m.Set("accounting_basis", basis)
	_ = m.Set("result", resp.toMap())
	return m
}

func joinReasons(parts []string) string { return strings.Join(parts, ", ") }
