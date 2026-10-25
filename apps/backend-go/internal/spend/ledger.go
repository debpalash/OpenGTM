package spend

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Admission is reserve_attempt's result dict.
type Admission struct {
	OK           bool
	Reason       string // set when !OK
	ID           string
	Status       string
	ContractHash string
	Reused       bool
}

// SettleResult is settle_attempt's result dict.
type SettleResult struct {
	OK     bool
	Reason string
	Reused bool
}

var million = big.NewRat(1000000, 1)

func nowEpoch() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// ratFromFloat is Decimal(str(f)): exact decimal of the shortest repr.
func ratFromFloat(f float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(f, 'e', -1, 64))
	return r
}

func ceilInt(r *big.Rat) *big.Int {
	q, m := new(big.Int).DivMod(r.Num(), r.Denom(), new(big.Int)) // floor for positive denominator
	if m.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func floorInt(r *big.Rat) *big.Int {
	return new(big.Int).Div(r.Num(), r.Denom())
}

// ExposureFromUSD is ceil(Decimal(str(cost)) * 1e6) as int64 micro-USD.
func ExposureFromUSD(usd float64) (int64, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return 0, invalid("cost must be a finite nonnegative number")
	}
	c := ceilInt(new(big.Rat).Mul(ratFromFloat(usd), million))
	if !c.IsInt64() {
		return 0, invalid("cost exceeds int64 micro-USD")
	}
	return c.Int64(), nil
}

func bigFromText(s string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("spend: bad sum %q", s)
	}
	return v, nil
}

// Reserve ports reserve_attempt. tx must be opened via tenancy.InWorkspace;
// the caller commits. The no-op workbook UPDATE serializes admissions.
func Reserve(ctx context.Context, tx pgx.Tx, r Reservation) (Admission, error) {
	if err := r.validate(); err != nil {
		return Admission{}, err
	}
	_, digest, err := ContractHash(r)
	if err != nil {
		return Admission{}, err
	}
	costBasis, err := canonjson.Marshal(r.CostBasis, true)
	if err != nil {
		return Admission{}, invalid("%v", err)
	}
	q := gen.New(tx)
	n, err := q.LockWorkbook(ctx, gen.LockWorkbookParams{WorkbookID: r.WorkbookID, WorkspaceID: r.WorkspaceID})
	if err != nil {
		return Admission{}, err
	}
	if n != 1 {
		return Admission{Reason: "workbook_not_found"}, nil
	}
	prev, err := q.GetAttemptByKey(ctx, gen.GetAttemptByKeyParams{WorkspaceID: r.WorkspaceID, AttemptKey: r.AttemptKey})
	switch {
	case err == nil:
		if prev.ContractHash != digest {
			return Admission{Reason: "contract_conflict"}, nil
		}
		return Admission{OK: true, ID: prev.ID, Status: prev.Status, ContractHash: digest, Reused: true}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Admission{}, err
	}
	budget, err := q.GetWorkbookBudget(ctx, gen.GetWorkbookBudgetParams{WorkbookID: r.WorkbookID, WorkspaceID: r.WorkspaceID})
	if err != nil {
		return Admission{}, err
	}
	outText, err := q.SumOutstandingMicroUSD(ctx, gen.SumOutstandingMicroUSDParams{WorkspaceID: r.WorkspaceID, WorkbookID: r.WorkbookID})
	if err != nil {
		return Admission{}, err
	}
	cellText, err := q.SumCellExposureMicroUSD(ctx, gen.SumCellExposureMicroUSDParams{WorkspaceID: r.WorkspaceID,
		WorkbookID: r.WorkbookID, RunID: r.RunID, RowIdentity: r.RowIdentity, ColumnID: r.ColumnID})
	if err != nil {
		return Admission{}, err
	}
	outstanding, err := bigFromText(outText)
	if err != nil {
		return Admission{}, err
	}
	cell, err := bigFromText(cellText)
	if err != nil {
		return Admission{}, err
	}
	exposure := big.NewInt(r.ExposureMicroUSD)
	if new(big.Int).Add(cell, exposure).Cmp(big.NewInt(r.CellLimitMicroUSD)) > 0 {
		return Admission{Reason: "cell_budget"}, nil
	}
	capF, spentF := 0.0, 0.0
	if budget.BudgetMaxUsd.Valid {
		capF = budget.BudgetMaxUsd.Float64
	}
	if budget.BudgetSpentUsd.Valid {
		spentF = budget.BudgetSpentUsd.Float64
	}
	if math.IsNaN(capF) || math.IsInf(capF, 0) || math.IsNaN(spentF) || math.IsInf(spentF, 0) || capF < 0 || spentF < 0 {
		return Admission{Reason: "invalid_workbook_budget"}, nil
	}
	if capF > 0 {
		capMicro := floorInt(new(big.Rat).Mul(ratFromFloat(capF), million))
		total := ceilInt(new(big.Rat).Mul(ratFromFloat(spentF), million))
		total.Add(total, outstanding).Add(total, exposure)
		if total.Cmp(capMicro) > 0 {
			return Admission{Reason: "workbook_budget"}, nil
		}
	}
	id, err := newUUID()
	if err != nil {
		return Admission{}, err
	}
	if err := q.InsertAttempt(ctx, gen.InsertAttemptParams{ID: id, WorkspaceID: r.WorkspaceID, WorkbookID: r.WorkbookID,
		RunID: r.RunID, RowIdentity: r.RowIdentity, ColumnID: r.ColumnID, Provider: r.Provider, AttemptKey: r.AttemptKey,
		ContractHash: digest, ReservedMicrousd: r.ExposureMicroUSD, CostBasis: costBasis, NowEpoch: nowEpoch()}); err != nil {
		return Admission{}, err
	}
	return Admission{OK: true, ID: id, Status: "reserved", ContractHash: digest}, nil
}

var transitions = map[string][2]string{
	"dispatch":                {"reserved", "dispatched"},
	"release_before_dispatch": {"reserved", "released"},
	"mark_uncertain":          {"dispatched", "uncertain"},
}

// Transition ports transition_attempt. It returns true only when this call
// applied the allowed transition; every other outcome (duplicate, wrong
// tenant, stale contract, lost lease) is (false, nil) and never authorizes a
// provider call.
func Transition(ctx context.Context, tx pgx.Tx, id Identity, action string) (bool, error) {
	t, ok := transitions[action]
	if !ok {
		return false, invalid("unsupported spend transition")
	}
	if err := id.validate(); err != nil {
		return false, err
	}
	q := gen.New(tx)
	var runID pgtype.Text
	if action == "dispatch" && id.Lease != nil {
		held, err := holdsLease(ctx, q, id)
		if err != nil || !held {
			return false, err
		}
		runID = pgtype.Text{String: fmt.Sprintf("job:%d", id.Lease.JobID), Valid: true}
	}
	n, err := q.TransitionAttempt(ctx, gen.TransitionAttemptParams{NextStatus: t[1], NowEpoch: nowEpoch(), ID: id.AttemptID,
		WorkspaceID: id.WorkspaceID, WorkbookID: id.WorkbookID, ContractHash: id.ContractHash, PrevStatus: t[0], RunID: runID})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// holdsLease ports batch_attempts._owned_job + fence_workbook_run_state: lock
// the job row (same strength as the Python no-op UPDATE) iff the lease is
// live, then require its payload to name this workbook.
func holdsLease(ctx context.Context, q *gen.Queries, id Identity) (bool, error) {
	l := id.Lease
	if l.JobID <= 0 || l.JobID > math.MaxInt32 || l.WorkerID == "" || l.LockedAt.IsZero() {
		return false, nil
	}
	payload, err := q.LockOwnedRunWorkbookJob(ctx, gen.LockOwnedRunWorkbookJobParams{ID: int32(l.JobID), WorkspaceID: id.WorkspaceID,
		WorkerID: l.WorkerID, LockedAt: pgtype.Timestamp{Time: l.LockedAt, Valid: true}, LeaseToken: l.LeaseToken})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var p struct {
		WorkbookID any `json:"workbook_id"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return false, nil
	}
	wb, isStr := p.WorkbookID.(string)
	return isStr && wb == id.WorkbookID, nil
}

var settleBases = map[string]bool{"catalog_estimate": true, "vendor_confirmed": true, "confirmed_nonbillable": true}

// Settle ports settle_attempt: persist the outcome and account the cost once.
func Settle(ctx context.Context, tx pgx.Tx, id Identity, chargedMicroUSD int64, basis string, result map[string]any) (SettleResult, error) {
	if chargedMicroUSD < 0 {
		return SettleResult{}, invalid("invalid settlement amount")
	}
	if !settleBases[basis] {
		return SettleResult{}, invalid("explicit accounting basis required")
	}
	if basis == "confirmed_nonbillable" && chargedMicroUSD != 0 {
		return SettleResult{}, invalid("nonbillable outcomes cannot have a charge")
	}
	if err := id.validate(); err != nil {
		return SettleResult{}, err
	}
	if result == nil {
		return SettleResult{}, invalid("settlement result is required")
	}
	receipt, err := canonjson.Marshal(map[string]any{"accounting_basis": basis, "result": result}, true)
	if err != nil {
		return SettleResult{}, invalid("%v", err)
	}
	q := gen.New(tx)
	n, err := q.LockWorkbook(ctx, gen.LockWorkbookParams{WorkbookID: id.WorkbookID, WorkspaceID: id.WorkspaceID})
	if err != nil {
		return SettleResult{}, err
	}
	if n != 1 {
		return SettleResult{Reason: "workbook_not_found"}, nil
	}
	att, err := q.LockAttemptForSettle(ctx, gen.LockAttemptForSettleParams{ID: id.AttemptID, WorkspaceID: id.WorkspaceID,
		WorkbookID: id.WorkbookID, ContractHash: id.ContractHash})
	if errors.Is(err, pgx.ErrNoRows) {
		return SettleResult{Reason: "attempt_not_found"}, nil
	}
	if err != nil {
		return SettleResult{}, err
	}
	switch att.Status {
	case "settled":
		if !att.SettledMicrousd.Valid || att.SettledMicrousd.Int64 != chargedMicroUSD || !jsonEqual(att.Result, receipt) {
			return SettleResult{Reason: "settlement_conflict"}, nil
		}
		return SettleResult{OK: true, Reused: true}, nil
	case "dispatched":
	default:
		return SettleResult{Reason: "invalid_state"}, nil
	}
	if err := q.MarkSettled(ctx, gen.MarkSettledParams{SettledMicrousd: pgtype.Int8{Int64: chargedMicroUSD, Valid: true},
		Result: receipt, NowEpoch: nowEpoch(), ID: id.AttemptID, WorkspaceID: id.WorkspaceID}); err != nil {
		return SettleResult{}, err
	}
	// Python: charged_microusd / 1000000 (correctly rounded true division).
	delta, _ := new(big.Rat).SetFrac64(chargedMicroUSD, 1000000).Float64()
	if err := q.AddWorkbookSpend(ctx, gen.AddWorkbookSpendParams{DeltaUsd: delta, WorkbookID: id.WorkbookID,
		WorkspaceID: id.WorkspaceID}); err != nil {
		return SettleResult{}, err
	}
	return SettleResult{OK: true}, nil
}

// jsonEqual compares two JSON documents with Python == semantics (numeric
// equality across int/float, True == 1).
func jsonEqual(a, b []byte) bool {
	na, err1 := normalize(a)
	nb, err2 := normalize(b)
	return err1 == nil && err2 == nil && fmt.Sprint(na) == fmt.Sprint(nb)
}

func normalize(raw []byte) (any, error) {
	if raw == nil {
		return nil, errors.New("null")
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return norm(v), nil
}

// norm yields a deterministic comparable form (fmt prints maps key-sorted).
func norm(v any) any {
	switch x := v.(type) {
	case json.Number:
		r, _ := new(big.Rat).SetString(x.String())
		return "n:" + r.RatString()
	case bool:
		if x {
			return "n:1"
		}
		return "n:0"
	case string:
		return "s:" + x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = norm(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = norm(e)
		}
		return out
	}
	return v
}

// ---- provider accounting (provider_accounting.accounting_envelope) ----

// ErrUncertainAccounting means the response does not establish whether a paid
// lookup was charged; the attempt must be marked uncertain, never assumed free.
var ErrUncertainAccounting = errors.New("uncertain provider accounting")

// ProviderResponse is the provider result dict; nil is "no response".
type ProviderResponse map[string]any

// Envelope is the accounting envelope handed to Settle.
type Envelope struct {
	ChargedMicroUSD int64
	AccountingBasis string
	Result          map[string]any
}

func uncertain(msg string) error { return fmt.Errorf("%w: %s", ErrUncertainAccounting, msg) }

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	}
	return 0, false // floats and bools are not Python ints here
}

func pyBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) && !(r >= 0x1c && r <= 0x1f) {
			return false
		}
	}
	return true
}

// AccountingEnvelope ports accounting_envelope.
func AccountingEnvelope(provider string, resp ProviderResponse, estimatedMicroUSD int64) (Envelope, error) {
	if estimatedMicroUSD < 0 {
		return Envelope{}, invalid("invalid catalog exposure")
	}
	success, successIsBool := resp["success"].(bool)
	if resp == nil || resp["provider"] != any(provider) || !successIsBool {
		return Envelope{}, uncertain("missing or mismatched provider result")
	}
	var amount int64
	var basis string
	if evidence := resp["billing_evidence"]; evidence != nil {
		ev, ok := evidence.(map[string]any)
		if !ok {
			return Envelope{}, uncertain("invalid billing evidence")
		}
		amt, amtOK := asInt(ev["charged_microusd"])
		ref, refOK := ev["reference"].(string)
		if !amtOK || amt < 0 || !refOK || pyBlank(ref) {
			return Envelope{}, uncertain("billing evidence needs amount and reference")
		}
		amount, basis = amt, "vendor_confirmed"
		if amt == 0 {
			basis = "confirmed_nonbillable"
		}
	} else if estimatedMicroUSD == 0 || success {
		amount, basis = estimatedMicroUSD, "catalog_estimate"
	} else {
		return Envelope{}, uncertain("paid unsuccessful lookup has no billing evidence")
	}
	return Envelope{ChargedMicroUSD: amount, AccountingBasis: basis, Result: deepCopy(map[string]any(resp)).(map[string]any)}, nil
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}
