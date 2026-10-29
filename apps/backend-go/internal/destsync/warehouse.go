package destsync

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/warehouse"
)

// flushWarehouse sends the buffered export once. With no rows buffered nothing is sent. Order:
// lease, cancel check, pending rows to in_flight (attempts+1, counted as attempted), the
// request with no transaction open, then every in_flight row of this run and destination
// settled to ONE terminal status; the counters are the affected row counts.
func (e *Engine) flushWarehouse(ctx context.Context, rs *runState, buf *warehouse.Buffer) (stop bool, err error) {
	if buf.RowCount() == 0 {
		return false, nil
	}
	if err := e.guard(ctx, rs); err != nil {
		return true, err
	}
	var cancelled bool
	var attempted int64
	e.tick(rs)
	err = e.inTx(ctx, rs, func(tx pgx.Tx) error {
		c, err := e.checkInTx(ctx, tx, rs)
		if err != nil || c {
			cancelled = c
			return err
		}
		attempted, err = gen.New(tx).DSWarehouseToInFlight(ctx, gen.DSWarehouseToInFlightParams{WorkspaceID: rs.ws, RunID: rs.runID, DestinationID: rs.dest.ID})
		return err
	})
	if err != nil || cancelled {
		return true, err
	}
	rs.st.attempted += int(attempted)

	res := e.callWarehouse(ctx, rs, buf)

	p := gen.DSWarehouseSettleParams{
		WorkspaceID: rs.ws, RunID: rs.runID, DestinationID: rs.dest.ID, Status: "failed",
		Summary: pgtype.Text{String: res.Summary, Valid: true}, Delivered: res.Success,
	}
	if res.Success {
		p.Status = "success"
	}
	if t := cleanError(res.Error); t != "" {
		p.Error = pgtype.Text{String: t, Valid: true}
	}
	if res.ExternalID != "" {
		p.ExternalID = pgtype.Text{String: res.ExternalID, Valid: true}
	}
	// Detached from the job context: a finished export is always recorded.
	sctx, cancel := settleContext(ctx)
	defer cancel()
	var affected int64
	err = e.inTx(sctx, rs, func(tx pgx.Tx) error {
		var err error
		affected, err = gen.New(tx).DSWarehouseSettle(sctx, p)
		return err
	})
	if err != nil {
		return true, fmt.Errorf("destsync: settle warehouse export: %w", err)
	}
	if res.Success {
		rs.st.succeeded += int(affected)
	} else {
		rs.st.failed += int(affected)
	}
	return false, ctx.Err()
}

func (e *Engine) callWarehouse(ctx context.Context, rs *runState, buf *warehouse.Buffer) (res warehouse.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = warehouse.Result{Error: "warehouse export failed unexpectedly"}
		}
	}()
	r, err := e.d.Warehouse.Sync(ctx, rs.ws, rs.ref, buf)
	if err != nil {
		return warehouse.Result{Error: truncRunes(cleanError(err.Error()), adErrorChars)}
	}
	return r
}
