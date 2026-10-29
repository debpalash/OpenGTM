package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// InsufficientCreditsError is Python's InsufficientCreditsError; the API maps it
// to 402 {"detail":{"message","balance_usd","required_usd"}}.
type InsufficientCreditsError struct{ BalanceUSD, RequiredUSD float64 }

// Error equals Python's InsufficientCreditsError.__str__ (both amounts rounded
// to 4 dp).
func (e *InsufficientCreditsError) Error() string {
	return fmt.Sprintf("Insufficient credits: balance $%.4f < required $%.4f. Top up to run this enrichment.",
		roundTo(e.BalanceUSD, 4), roundTo(e.RequiredUSD, 4))
}

// debitKeys returns the idempotency key written for a run and the legacy
// Python key. Python's key "run:<run_id>" is global across workspaces (the same
// run id in a second workspace is silently treated as already charged); Go
// scopes it per workspace (documented security hardening, README deviation).
// The column is varchar(160): an over-long scoped key is hashed.
func debitKeys(workspaceID, runID string) (scoped, legacy string) {
	legacy = "run:" + runID
	scoped = legacy + "@ws:" + workspaceID
	if len(scoped) > 160 {
		sum := sha256.Sum256([]byte(runID))
		scoped = "run:sha256:" + hex.EncodeToString(sum[:]) + "@ws:" + workspaceID
	}
	return scoped, legacy
}

// CheckAndDebit enforces billing at the pre-run chokepoint (service.py:
// check_and_debit). It runs inside the caller's transaction, which must be
// scoped to workspaceID, and never commits: a later failure in the caller (for
// example the enqueue) rolls the debit back.
func CheckAndDebit(ctx context.Context, tx pgx.Tx, enabled bool, workspaceID string, projectedUSD float64, runID, reason string) (DebitResult, error) {
	noop := func() (DebitResult, error) {
		bal, err := Balance(ctx, tx, workspaceID)
		return DebitResult{BalanceUSD: bal}, err
	}
	if !enabled {
		return noop()
	}
	cost := roundTo(projectedUSD, 4)
	// NaN is never chargeable (Python would write a NaN ledger row).
	if cost <= 0 || math.IsNaN(cost) {
		return noop()
	}

	q := gen.New(tx)
	// The row lock serializes debits of one workspace, so the key check below
	// sees every debit that committed while this transaction waited.
	current, err := q.BillingLockBalance(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		current = 0
	} else if err != nil {
		return DebitResult{}, fmt.Errorf("billing: lock balance: %w", err)
	}
	scoped, legacy := debitKeys(workspaceID, runID)
	replay := DebitResult{BalanceUSD: roundTo(current, 4), IdempotentReplay: true}
	found, err := q.BillingDebitKeyExists(ctx, []string{scoped, legacy})
	if err != nil {
		return DebitResult{}, fmt.Errorf("billing: idempotency lookup: %w", err)
	}
	if found {
		return replay, nil
	}
	if current < cost {
		return DebitResult{}, &InsufficientCreditsError{BalanceUSD: roundTo(current, 4), RequiredUSD: cost}
	}

	// A savepoint keeps the caller's transaction usable if the unique key still
	// collides (belt and braces: the balance lock already serializes).
	sp, err := tx.Begin(ctx)
	if err != nil {
		return DebitResult{}, fmt.Errorf("billing: savepoint: %w", err)
	}
	err = gen.New(sp).BillingInsertDebit(ctx, gen.BillingInsertDebitParams{
		WorkspaceID: workspaceID, AmountUsd: -cost, Reason: reason, RunID: runID, IdempotencyKey: scoped,
	})
	if err != nil {
		_ = sp.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			// Only a replay if the colliding key is visible in this workspace;
			// a key held by another tenant is never a free replay.
			seen, qerr := q.BillingDebitKeyExists(ctx, []string{scoped, legacy})
			if qerr == nil && seen {
				return replay, nil
			}
			return DebitResult{}, fmt.Errorf("billing: debit idempotency key collision not visible in workspace: %w", err)
		}
		return DebitResult{}, fmt.Errorf("billing: insert debit: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return DebitResult{}, fmt.Errorf("billing: release savepoint: %w", err)
	}
	next := roundTo(current-cost, 6)
	if err := q.BillingSetBalance(ctx, gen.BillingSetBalanceParams{WorkspaceID: workspaceID, BalanceUsd: next}); err != nil {
		return DebitResult{}, fmt.Errorf("billing: update balance: %w", err)
	}
	return DebitResult{ChargedUSD: cost, BalanceUSD: roundTo(next, 4)}, nil
}

// CostCatalog is the slice of the vendor catalog the projection needs.
type CostCatalog interface {
	// PlatformCost returns the per-lookup USD cost of a platform-billed
	// provider; ok is false for free providers and BYOK providers (user's own
	// key, never debited).
	PlatformCost(provider string) (usd float64, ok bool)
}

// ResearchVendorCost reports the RESEARCH_VENDOR_COST flag (default off).
// Python reads it from workspace-wide settings, falling back to the
// environment; tests override this variable.
var ResearchVendorCost = func() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RESEARCH_VENDOR_COST"))) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

const researchBaseCost = 0.05

// researchOnly is the default catalog: in Python's catalog the only
// platform-billed (byok=False) vendor is "research", at 0.05 behind
// RESEARCH_VENDOR_COST. Task 19 (runplan) swaps in the full vendorcatalog via
// SetCostCatalog without touching callers.
type researchOnly struct{}

func (researchOnly) PlatformCost(p string) (float64, bool) {
	if p == "research" && ResearchVendorCost() {
		return researchBaseCost, true
	}
	return 0, false
}

var catalog CostCatalog = researchOnly{}

// SetCostCatalog replaces the cost source used by ProjectedPlatformCost (call
// once at wiring time, before serving).
func SetCostCatalog(c CostCatalog) {
	if c != nil {
		catalog = c
	}
}

// ProjectedPlatformCost is worst-case platform-billed spend for a run
// (service.py:projected_platform_cost): every platform-billed provider of a
// column's chain counted once per row, BYOK and free providers excluded. Exact
// decimal arithmetic (Python uses Decimal(str(cost)) * rows), rounded half-even
// to 4 dp. columnCounts nil means numRows for every column; a column missing
// from a non-nil map counts 0; negatives clamp to 0.
func ProjectedPlatformCost(numRows int, providersByCol map[string][]string, columnCounts map[string]int) float64 {
	total := new(big.Rat)
	for col, providers := range providersByCol {
		count := numRows
		if columnCounts != nil {
			count = columnCounts[col]
		}
		if count < 0 {
			count = 0
		}
		if count == 0 {
			continue
		}
		for _, p := range providers {
			usd, ok := catalog.PlatformCost(p)
			if !ok || !(usd > 0) {
				continue
			}
			c, okr := new(big.Rat).SetString(strconv.FormatFloat(usd, 'g', -1, 64))
			if !okr {
				continue
			}
			total.Add(total, c.Mul(c, new(big.Rat).SetInt64(int64(count))))
		}
	}
	// round half-even to 4 dp: total*10^4 -> integer.
	scaled := new(big.Rat).Mul(total, big.NewRat(10000, 1))
	num, den := scaled.Num(), scaled.Denom()
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	twice := new(big.Int).Mul(r, big.NewInt(2))
	switch twice.Cmp(den) {
	case 1:
		q.Add(q, big.NewInt(1))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(1))
		}
	}
	f, err := strconv.ParseFloat(new(big.Rat).SetFrac(q, big.NewInt(10000)).FloatString(4), 64)
	if err != nil {
		return 0
	}
	return f
}
