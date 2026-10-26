package poller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

func utcDay(t time.Time) pgtype.Date {
	u := t.UTC()
	return pgtype.Date{Time: time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
}

// DebitBudget is _debit_budget: one atomic INSERT ... ON CONFLICT DO UPDATE
// ... RETURNING on the (workspace, UTC day) ledger row. Concurrent callers are
// serialised by the row lock, so each sees a distinct n and a daily budget
// check on the result (n > budget) can never admit more polls than the budget.
// A poll-now debit raises both the shared counter (closing the bypass) and the
// manual-poll counter. tx must be inside tenancy.InWorkspace.
func DebitBudget(ctx context.Context, tx pgx.Tx, ws string, pollNow bool, today time.Time) (n, nPollNow int, err error) {
	var pn int32
	if pollNow {
		pn = 1
	}
	r, err := gen.New(tx).PollDebitBudget(ctx, gen.PollDebitBudgetParams{WorkspaceID: ws, Day: utcDay(today), PollNow: pn})
	if err != nil {
		return 0, 0, fmt.Errorf("poller: debit budget: %w", err)
	}
	return int(r.N), int(r.NPollNow), nil
}

// OverBudget reports whether a post-increment count exceeds a daily budget
// (budget <= 0 means unlimited).
func OverBudget(n, budget int) bool { return budget > 0 && n > budget }

// PollNowQuotaUsed is poll_now_quota_used: today's manual polls (0 when the
// ledger row does not exist yet).
func PollNowQuotaUsed(ctx context.Context, tx pgx.Tx, ws string, today time.Time) (int, error) {
	n, err := gen.New(tx).PollNowQuotaUsed(ctx, gen.PollNowQuotaUsedParams{WorkspaceID: ws, Day: utcDay(today)})
	if err != nil {
		return 0, fmt.Errorf("poller: poll-now quota: %w", err)
	}
	return int(n), nil
}

// billingSource maps a poll source to the billing bucket _poll_one_source
// uses: SEC sources are "funding", feed/job_change/account_group stand alone,
// everything else (jobspy tech, web_tech) bills as "hiring".
func billingSource(source string) string {
	switch source {
	case "funding", "exec", "funding_exec":
		return "funding"
	case "feed", "job_change", "account_group":
		return source
	}
	return "hiring"
}

func (c Config) sourceCost(bucket string) float64 {
	switch bucket {
	case "funding":
		return c.FundingCostUSD
	case "hiring":
		return c.HiringCostUSD
	case "feed":
		return c.FeedCostUSD
	}
	return 0 // job_change and account_group are keyless (free)
}

// DebitSource is _debit_source: charge the per-source fetch cost BEFORE a paid
// fetch. The run id is "<fire_key>:<billing source>", so a retry of the same
// poll never charges twice (billing.CheckAndDebit is idempotent per run id and
// serialises on the workspace balance row). proceed is false when credits are
// insufficient; that is a source failure for the caller, not a healthy poll.
//
// Documented deviation: in Python any POLLER_*_COST_USD > 0 crashes this gate,
// so the paid path never ran; Go implements the intended gate. With every cost
// at its default 0 (or billing off) this never touches the ledger. tx must be
// inside tenancy.InWorkspace and the debit commits or rolls back with it.
func DebitSource(ctx context.Context, tx pgx.Tx, cfg Config, ws, source, fireKey string) (proceed bool, err error) {
	bucket := billingSource(source)
	cost := cfg.sourceCost(bucket)
	if !(cost > 0) { // also false for NaN
		return true, nil
	}
	_, err = billing.CheckAndDebit(ctx, tx, cfg.BillingEnabled, ws, cost, fireKey+":"+bucket, "poller_fetch")
	var insufficient *billing.InsufficientCreditsError
	if errors.As(err, &insufficient) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
