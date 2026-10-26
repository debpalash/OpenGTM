package destsync

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
)

// tick counts one cancellation check (and lets a test inject a cancel request just before it).
// It runs before the check's transaction opens.
func (e *Engine) tick(rs *runState) {
	if e.checkHook != nil {
		e.checkHook(rs.checks)
	}
	rs.checks++
}

// checkInTx is _cancellation_requested followed by _finish_cancelled: the run is re-read and,
// when an operator asked it to stop (cancelling, or already cancelled), the run is ended
// cancelled in this same transaction: open deliveries become cancelled, completed effects are
// kept, counters are recomputed from the ledger. cancelled=true tells the caller to stop.
func (e *Engine) checkInTx(ctx context.Context, tx pgx.Tx, rs *runState) (cancelled bool, err error) {
	run, err := deststore.GetRun(ctx, tx, rs.runID)
	if err != nil {
		return false, err
	}
	if run.Status != "cancelling" && run.Status != "cancelled" {
		return false, nil
	}
	if _, err := deststore.FinishCancelled(ctx, tx, run, &rs.dest); err != nil {
		return false, err
	}
	e.d.Log.Info("destination sync cancelled", "run_id", rs.runID)
	return true, nil
}
