package deststore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// ActiveStatuses are the run states that block a new sync of the destination.
var ActiveStatuses = []string{"pending", "running", "cancelling"}

// GetRun is the workspace-scoped run lookup (ErrRunNotFound for another tenant's id).
func GetRun(ctx context.Context, tx pgx.Tx, id string) (Run, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return Run{}, err
	}
	row, err := gen.New(tx).DestRunGet(ctx, gen.DestRunGetParams{WorkspaceID: ws, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrRunNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("deststore: get run: %w", err)
	}
	return runFrom(row), nil
}

// lockRun reads the run FOR UPDATE so concurrent state changes serialise.
func lockRun(ctx context.Context, q *gen.Queries, ws, id string) (Run, error) {
	row, err := q.DestRunLock(ctx, gen.DestRunLockParams{WorkspaceID: ws, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrRunNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("deststore: lock run: %w", err)
	}
	return runFrom(row), nil
}

// ActiveRun is the destination's pending, running or cancelling run, if any.
func ActiveRun(ctx context.Context, tx pgx.Tx, destID string) (*Run, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return nil, err
	}
	return activeRun(ctx, gen.New(tx), ws, destID)
}

func activeRun(ctx context.Context, q *gen.Queries, ws, destID string) (*Run, error) {
	row, err := q.DestRunActive(ctx, gen.DestRunActiveParams{WorkspaceID: ws, DestinationID: destID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("deststore: active run: %w", err)
	}
	r := runFrom(row)
	return &r, nil
}

// DeliveryCounts aggregates a run's ledger in SQL (attempted = rows with attempts > 0).
type DeliveryCounts struct{ Attempted, Succeeded, Failed, Skipped int }

// CountDeliveries is _delivery_counts.
func CountDeliveries(ctx context.Context, tx pgx.Tx, runID string) (DeliveryCounts, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return DeliveryCounts{}, err
	}
	return countDeliveries(ctx, gen.New(tx), ws, runID)
}

func countDeliveries(ctx context.Context, q *gen.Queries, ws, runID string) (DeliveryCounts, error) {
	c, err := q.DestDeliveryCounts(ctx, gen.DestDeliveryCountsParams{WorkspaceID: ws, RunID: runID})
	if err != nil {
		return DeliveryCounts{}, fmt.Errorf("deststore: delivery counts: %w", err)
	}
	return DeliveryCounts{int(c.Attempted), int(c.Succeeded), int(c.Failed), int(c.Skipped)}, nil
}

// FinishCancelled is _finish_cancelled: open (pending / in_flight) deliveries become
// cancelled with "Run cancelled before delivery", the run's counts are recomputed
// from the ledger and it ends cancelled ("Cancellation completed"); completed
// effects are kept and, when something succeeded, dest.last_success_at moves to the
// finish time. dest may be nil (the destination is gone).
func FinishCancelled(ctx context.Context, tx pgx.Tx, run Run, dest *Destination) (Run, error) {
	if err := requireTenant(ctx, tx, run.WorkspaceID); err != nil {
		return Run{}, err
	}
	return finishCancelled(ctx, gen.New(tx), run.WorkspaceID, run.ID, dest)
}

func finishCancelled(ctx context.Context, q *gen.Queries, ws, runID string, dest *Destination) (Run, error) {
	if err := q.DestDeliveriesCancelOpen(ctx, gen.DestDeliveriesCancelOpenParams{WorkspaceID: ws, RunID: runID}); err != nil {
		return Run{}, fmt.Errorf("deststore: cancel open deliveries: %w", err)
	}
	counts, err := countDeliveries(ctx, q, ws, runID)
	if err != nil {
		return Run{}, err
	}
	row, err := q.DestRunSetFinished(ctx, gen.DestRunSetFinishedParams{
		Status: "cancelled", Error: "Cancellation completed", WorkspaceID: ws, ID: runID,
		Attempted: int32(counts.Attempted), Succeeded: int32(counts.Succeeded), Failed: int32(counts.Failed), Skipped: int32(counts.Skipped),
	})
	if err != nil {
		return Run{}, fmt.Errorf("deststore: finish cancelled run: %w", err)
	}
	if dest != nil && counts.Succeeded > 0 {
		if err := q.DestSetLastSuccess(ctx, gen.DestSetLastSuccessParams{WorkspaceID: ws, ID: dest.ID, LastSuccessAt: row.FinishedAt}); err != nil {
			return Run{}, fmt.Errorf("deststore: set last_success_at: %w", err)
		}
	}
	return runFrom(row), nil
}

// destinationOrNil loads the run's destination; a missing one is nil.
func destinationOrNil(ctx context.Context, q *gen.Queries, ws, id string) (*Destination, error) {
	d, err := getDestination(ctx, q, ws, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// CancelPendingRun cancels a run that has not started (cancel_run, pending branch):
// finished immediately. Any other status is ErrNotCancellable.
func CancelPendingRun(ctx context.Context, tx pgx.Tx, runID string) (Run, error) {
	r, _, err := cancel(ctx, tx, runID, true)
	return r, err
}

// RequestCancel asks a running (or already cancelling) run to stop at the next safe
// delivery boundary: status cancelling, "Cancellation requested". Pending runs and
// terminal ones are ErrNotCancellable.
func RequestCancel(ctx context.Context, tx pgx.Tx, runID string) (Run, error) {
	r, _, err := cancel(ctx, tx, runID, false)
	return r, err
}

// CancelRun is the whole cancel_run route body: pending runs finish at once, running
// and cancelling ones are asked to stop. It returns the run and its previous status
// (for the audit metadata); terminal runs are ErrNotCancellable.
func CancelRun(ctx context.Context, tx pgx.Tx, runID string) (Run, string, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return Run{}, "", err
	}
	cur, err := lockRun(ctx, gen.New(tx), ws, runID)
	if err != nil {
		return Run{}, "", err
	}
	return cancelLocked(ctx, tx, ws, cur, cur.Status == "pending")
}

func cancel(ctx context.Context, tx pgx.Tx, runID string, wantPending bool) (Run, string, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return Run{}, "", err
	}
	cur, err := lockRun(ctx, gen.New(tx), ws, runID)
	if err != nil {
		return Run{}, "", err
	}
	if wantPending != (cur.Status == "pending") {
		return Run{}, cur.Status, ErrNotCancellable
	}
	return cancelLocked(ctx, tx, ws, cur, wantPending)
}

func cancelLocked(ctx context.Context, tx pgx.Tx, ws string, cur Run, pending bool) (Run, string, error) {
	q := gen.New(tx)
	switch cur.Status {
	case "pending", "running", "cancelling":
	default:
		return Run{}, cur.Status, ErrNotCancellable
	}
	if pending {
		dest, err := destinationOrNil(ctx, q, ws, cur.DestinationID)
		if err != nil {
			return Run{}, "", err
		}
		r, err := finishCancelled(ctx, q, ws, cur.ID, dest)
		return r, cur.Status, err
	}
	row, err := q.DestRunRequestCancel(ctx, gen.DestRunRequestCancelParams{WorkspaceID: ws, ID: cur.ID})
	if err != nil {
		return Run{}, "", fmt.Errorf("deststore: request cancel: %w", err)
	}
	return runFrom(row), cur.Status, nil
}

// RunsCursor is a decoded history cursor: the last run's created_at (aware UTC)
// and id; the next page is strictly older.
type RunsCursor struct {
	CreatedAt time.Time
	ID        string
}

// RunsQuery is one page request. Offset is ignored when a cursor is given.
type RunsQuery struct {
	Limit  int
	Offset int
	Cursor *RunsCursor
}

// RunsPage is limit runs plus whether more exist (the limit+1 probe row is dropped).
type RunsPage struct {
	Runs    []Run
	HasMore bool
}

// ListRuns pages a destination's runs newest first (created_at DESC, id DESC).
// ErrNotFound when the destination is not in the workspace.
func ListRuns(ctx context.Context, tx pgx.Tx, destID string, q RunsQuery) (RunsPage, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return RunsPage{}, err
	}
	if q.Limit < 1 || q.Offset < 0 || q.Limit > 1<<20 || q.Offset > 1<<30 {
		return RunsPage{}, fmt.Errorf("deststore: bad page request limit=%d offset=%d", q.Limit, q.Offset)
	}
	g := gen.New(tx)
	if _, err := getDestination(ctx, g, ws, destID); err != nil {
		return RunsPage{}, err
	}
	p := gen.DestRunsListParams{WorkspaceID: ws, DestinationID: destID, RowLimit: int32(q.Limit + 1)}
	if q.Cursor != nil {
		p.HasCursor = true
		p.CursorCreatedAt = pgtype.Timestamptz{Time: q.Cursor.CreatedAt.UTC(), Valid: true}
		p.CursorID = q.Cursor.ID
	} else {
		p.RowOffset = int32(q.Offset)
	}
	rows, err := g.DestRunsList(ctx, p)
	if err != nil {
		return RunsPage{}, fmt.Errorf("deststore: list runs: %w", err)
	}
	page := RunsPage{HasMore: len(rows) > q.Limit}
	if page.HasMore {
		rows = rows[:q.Limit]
	}
	page.Runs = make([]Run, 0, len(rows))
	for _, r := range rows {
		page.Runs = append(page.Runs, runFrom(r))
	}
	return page, nil
}
