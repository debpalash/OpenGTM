package workbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Single-cell output delivery journal (plan 15 review M2). A queued run journals a delivery in
// its job payload (output_attempts); an in-request run has no job, so the same claim / receipt
// protocol lives on the cell itself: under a per-cell advisory lock the run claims the cell
// (status running + cell_metadata.output_claim) before it contacts the destination, and the
// receipt is the cell write that replaces the claim. A claim without a receipt (a crashed
// run) is never resent silently: it ends in output_delivery_requires_review, and only force
// re-sends. While a run_workbook job of the workbook is processing, the in-request run is
// refused because that job delivers the same cells under its own journal.
//
// Go-only deviation from Python (which sends directly, so a double click delivers twice).

const (
	codeOutputInProgress   = "output_run_in_progress"
	codeWorkbookInProgress = "workbook_run_in_progress"
	codeOutputReview       = "output_delivery_requires_review"
	// outputClaimLease is how long a claim blocks other runs: the detached run budget plus the
	// destination's own timeouts. A claim older than that is a crashed run.
	outputClaimLease = cellRunBudget + time.Minute
)

// outputRefusal is a run that must not deliver and must not touch the cell: another delivery
// owns it.
type outputRefusal struct{ code string }

func (e *outputRefusal) Error() string { return "workbook: output run refused: " + e.code }

// executeOutputSingle delivers one cell of an in-request run under the cell claim.
func (rs *runState) executeOutputSingle(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (outResult, error) {
	ws, wb := rs.p.WorkspaceID, rs.p.WorkbookID
	key := fmt.Sprintf("output:%s:%s:%s", ws, wb, outputCellKey(lead, col, leadID))
	var review bool
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, key); err != nil {
			return err
		}
		q := gen.New(tx)
		if _, err := q.ProcessingRunWorkbookJobID(ctx, gen.ProcessingRunWorkbookJobIDParams{WorkspaceID: ws, WorkbookID: wb}); err == nil {
			return &outputRefusal{code: codeWorkbookInProgress}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row, err := q.GetOutputCellClaim(ctx, gen.GetOutputCellClaimParams{WorkbookID: wb, WorkspaceID: ws,
			LeadID: int32(leadID), ColumnID: col.ID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && row.Status.Valid && row.Status.String == "running" {
			if at, ok := claimTime(row.CellMetadata); ok {
				if time.Since(at) < outputClaimLease {
					return &outputRefusal{code: codeOutputInProgress}
				}
				review = true // a crashed run: delivery state unknown
			}
		}
		if review && !rs.forceOutput {
			return nil // the caller writes the review error cell
		}
		meta, _ := json.Marshal(map[string]any{"output_claim": map[string]any{"claimed_at": time.Now().UTC().Format(time.RFC3339)}})
		return q.UpsertEnrichment(ctx, gen.UpsertEnrichmentParams{WorkbookID: wb, WorkspaceID: ws, LeadID: int32(leadID),
			ColumnID: col.ID, Status: "running", CellMetadata: meta})
	})
	if err != nil {
		return outResult{}, err
	}
	if review && !rs.forceOutput {
		return outResult{Error: codeOutputReview}, nil
	}
	return rs.executeOutput(ctx, lead, col, leadID)
}

func claimTime(meta string) (time.Time, bool) {
	var m struct {
		Claim struct {
			At string `json:"claimed_at"`
		} `json:"output_claim"`
	}
	if json.Unmarshal([]byte(meta), &m) != nil || m.Claim.At == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, m.Claim.At)
	return t, err == nil
}
