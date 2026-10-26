// Package collectledger is the Postgres collection job/stage ledger (port of
// PgJobLedger, job_ledger.py): the rows behind /api/jobs. Every function runs in a
// tenancy.InWorkspace transaction; the tables are FORCE RLS and each statement also
// filters workspace_id from the transaction-local GUC. Timestamps stay the TEXT
// values Python stores (datetime.isoformat strings), JSON columns stay TEXT.
package collectledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

var (
	// ErrNotFound: no such job in the transaction's workspace (another workspace's
	// job looks the same). The routes answer 404 "Job not found".
	ErrNotFound = errors.New("collectledger: job not found")
	// ErrTenantMismatch: the workspace argument differs from the transaction's tenant.
	ErrTenantMismatch = errors.New("collectledger: workspace is not the transaction tenant")
)

// DefaultListLimit is PgJobLedger.get_jobs' default limit.
const DefaultListLimit = 50

// Isoformat renders t like Python's datetime.isoformat(): microsecond resolution
// (truncated), the fraction omitted when zero, and a +HH:MM[:SS] UTC offset.
func Isoformat(t time.Time) string {
	s := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second())
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	_, off := t.Zone()
	sign := '+'
	if off < 0 {
		sign, off = '-', -off
	}
	s += fmt.Sprintf("%c%02d:%02d", sign, off/3600, off%3600/60)
	if sec := off % 60; sec != 0 {
		s += fmt.Sprintf(":%02d", sec)
	}
	return s
}

// requireTenant fails unless ws is the tenant the transaction is scoped to.
func requireTenant(ctx context.Context, tx pgx.Tx, ws string) error {
	var cur string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.workspace_id', true), '')`).Scan(&cur); err != nil {
		return fmt.Errorf("collectledger: read tenant: %w", err)
	}
	if ws == "" || cur != ws {
		return ErrTenantMismatch
	}
	return nil
}

// Create inserts a pending job (ON CONFLICT (id) DO NOTHING: a repeated id keeps the
// first row, like PgJobLedger.create_job). ws must be the transaction's tenant.
func Create(ctx context.Context, tx pgx.Tx, ws, id, query, intent, intentDetails string, now time.Time) error {
	if err := requireTenant(ctx, tx, ws); err != nil {
		return err
	}
	_, err := gen.New(tx).CJCreate(ctx, gen.CJCreateParams{
		ID: id, Query: query, Intent: intent, IntentDetails: intentDetails, CreatedAt: Isoformat(now),
	})
	if err != nil {
		return fmt.Errorf("collectledger: create: %w", err)
	}
	return nil
}

// Cancel moves a running or pending job to cancelled (error 'Cancelled by user',
// completed_at = now.isoformat()). changed is false when the guard fails, the job is
// missing or belongs to another workspace; Python reports none of these either.
func Cancel(ctx context.Context, tx pgx.Tx, id string, now time.Time) (changed bool, err error) {
	_, err = gen.New(tx).CJCancel(ctx, gen.CJCancelParams{ID: id, CompletedAt: Isoformat(now)})
	return guarded(err, "cancel")
}

// Retry resets a failed or cancelled job to pending (error and completed_at cleared,
// attempts 0). changed is false when the guard fails or the job is not visible.
func Retry(ctx context.Context, tx pgx.Tx, id string) (changed bool, err error) {
	_, err = gen.New(tx).CJRetry(ctx, id)
	return guarded(err, "retry")
}

func guarded(err error, op string) (bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("collectledger: %s: %w", op, err)
	}
	return true, nil
}

// Delete removes the job; its stages go with it (ON DELETE CASCADE). The job's leads
// are removed separately (gen.CJDeleteLeads) unless the caller keeps them.
func Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := gen.New(tx).CJDelete(ctx, id); err != nil {
		return fmt.Errorf("collectledger: delete: %w", err)
	}
	return nil
}

// MarkEnqueueFailed is PgJobLedger.mark_failed: status failed, error msg verbatim
// (the route prefixes "Queue enqueue failed: " / "Retry enqueue failed: "),
// completed_at = now.isoformat().
func MarkEnqueueFailed(ctx context.Context, tx pgx.Tx, id, msg string, now time.Time) error {
	_, err := gen.New(tx).CJMarkFailedEnqueue(ctx, gen.CJMarkFailedEnqueueParams{ID: id, Error: msg, CompletedAt: Isoformat(now)})
	if err != nil {
		return fmt.Errorf("collectledger: mark failed: %w", err)
	}
	return nil
}
