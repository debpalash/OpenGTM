// Package billing is the credit ledger and the Stripe integration (top-ups).
// It is the only package that imports stripe-go.
package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// ErrNonPositiveCredit is returned for a credit amount <= 0 (Python's
// ValueError("credit amount must be positive")).
var ErrNonPositiveCredit = errors.New("credit amount must be positive")

// DebitResult mirrors services/billing/service.py:DebitResult.
type DebitResult struct {
	ChargedUSD       float64
	BalanceUSD       float64
	IdempotentReplay bool
}

// Entry is one ledger row as the API renders it (amount rounded to 4 dp).
type Entry struct {
	ID          int32
	WorkspaceID string
	AmountUSD   float64
	Reason      string
	RunID       *string
	CreatedAt   *time.Time // naive UTC, nil when NULL
}

// roundTo rounds like Python's round(x, n): the correctly rounded decimal of
// the exact binary value (ties to even), not x*10^n arithmetic.
func roundTo(x float64, n int) float64 {
	v, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	if err != nil {
		return x
	}
	return v
}

const uniqueViolation = "23505"

// Credit adds amountUSD to the workspace balance inside tx (which must already
// be scoped to workspaceID, i.e. run under tenancy.InWorkspace). With a
// non-empty idemKey a repeat is a no-op replay. Behaviour of
// services/billing/service.py:credit.
func Credit(ctx context.Context, tx pgx.Tx, workspaceID string, amountUSD float64, reason, idemKey string) (DebitResult, error) {
	if !(amountUSD > 0) || math.IsInf(amountUSD, 0) {
		return DebitResult{}, ErrNonPositiveCredit
	}
	q := gen.New(tx)
	if err := q.BillingEnsureCredits(ctx, workspaceID); err != nil {
		return DebitResult{}, fmt.Errorf("billing: ensure balance: %w", err)
	}
	// The row lock serializes credits per workspace; the key check below then
	// sees any credit that committed while this transaction waited.
	bal, err := q.BillingLockBalance(ctx, workspaceID)
	if err != nil {
		return DebitResult{}, fmt.Errorf("billing: lock balance: %w", err)
	}
	replay := DebitResult{BalanceUSD: roundTo(bal, 4), IdempotentReplay: true}
	if idemKey != "" {
		found, err := q.BillingLedgerKeyExists(ctx, idemKey)
		if err != nil {
			return DebitResult{}, fmt.Errorf("billing: idempotency lookup: %w", err)
		}
		if found {
			return replay, nil
		}
	}

	// The unique key spans tenants while RLS hides other tenants' rows, so the
	// insert can still collide; a savepoint keeps the transaction usable.
	var key *string
	if idemKey != "" {
		key = &idemKey
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return DebitResult{}, fmt.Errorf("billing: savepoint: %w", err)
	}
	err = gen.New(sp).BillingInsertEntry(ctx, gen.BillingInsertEntryParams{
		WorkspaceID: workspaceID, AmountUsd: amountUSD, Reason: reason, IdempotencyKey: textOf(key),
	})
	if err != nil {
		_ = sp.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && key != nil {
			return replay, nil
		}
		return DebitResult{}, fmt.Errorf("billing: insert entry: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return DebitResult{}, fmt.Errorf("billing: release savepoint: %w", err)
	}
	next := roundTo(bal+amountUSD, 6)
	if err := q.BillingSetBalance(ctx, gen.BillingSetBalanceParams{WorkspaceID: workspaceID, BalanceUsd: next}); err != nil {
		return DebitResult{}, fmt.Errorf("billing: update balance: %w", err)
	}
	return DebitResult{ChargedUSD: amountUSD, BalanceUSD: roundTo(next, 4)}, nil
}

// Balance returns the workspace balance rounded to 4 dp (0 when no row).
func Balance(ctx context.Context, tx pgx.Tx, workspaceID string) (float64, error) {
	bal, err := gen.New(tx).BillingGetBalance(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("billing: balance: %w", err)
	}
	return roundTo(bal, 4), nil
}

// RecentEntries returns the newest ledger rows first, limit clamped to 1..500.
func RecentEntries(ctx context.Context, tx pgx.Tx, workspaceID string, limit int) ([]Entry, error) {
	limit = max(1, min(limit, 500))
	rows, err := gen.New(tx).BillingRecentEntries(ctx, gen.BillingRecentEntriesParams{WorkspaceID: workspaceID, RowLimit: int32(limit)})
	if err != nil {
		return nil, fmt.Errorf("billing: entries: %w", err)
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		e := Entry{ID: r.ID, WorkspaceID: r.WorkspaceID, AmountUSD: roundTo(r.AmountUsd, 4), Reason: r.Reason}
		if r.RunID.Valid {
			s := r.RunID.String
			e.RunID = &s
		}
		if r.CreatedAt.Valid {
			t := r.CreatedAt.Time
			e.CreatedAt = &t
		}
		out = append(out, e)
	}
	return out, nil
}

func textOf(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}
