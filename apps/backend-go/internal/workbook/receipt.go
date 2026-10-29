package workbook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// PersistReceipt ports run_receipts.persist_run_result: merge
// payload["execution_result"] into the job row, but only for the queue lease
// that produced it (processing or cancelled, same worker/token/locked_at). It
// returns false when the lease no longer matches; that is not an error.
func PersistReceipt(ctx context.Context, deps Deps, c *queue.Claim, res Result) (bool, error) {
	if res.skipReceipt || res.Completed < 0 || res.Errors < 0 || res.Total < 0 || res.Rows < 0 {
		return false, nil
	}
	var p struct {
		WorkspaceID string `json:"workspace_id"`
		WorkbookID  string `json:"workbook_id"`
	}
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		return false, fmt.Errorf("workbook: receipt payload: %w", err)
	}
	if p.WorkspaceID == "" || p.WorkbookID == "" || c.WorkerID == "" || c.LockedAt.IsZero() {
		return false, nil
	}
	if c.LeaseToken <= 0 {
		return false, errors.New("queue lease missing lease_token")
	}
	ts := pgtype.Timestamp{Time: c.LockedAt, Valid: true}
	var changed int64
	err := tenancy.InWorkspace(ctx, deps.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		job, err := q.LockLeaseForReceipt(ctx, gen.LockLeaseForReceiptParams{ID: int32(c.ID), WorkspaceID: c.WorkspaceID,
			WorkerID: c.WorkerID, LeaseToken: c.LeaseToken, LockedAt: ts})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		merged, ok, err := mergeReceipt(job.Payload, p.WorkbookID, res, int(job.RetryCount.Int32), time.Now().UTC())
		if err != nil || !ok {
			return err
		}
		changed, err = q.StoreReceiptPayload(ctx, gen.StoreReceiptPayloadParams{Payload: merged, ID: int32(c.ID),
			WorkspaceID: c.WorkspaceID, WorkerID: c.WorkerID, LeaseToken: c.LeaseToken, LockedAt: ts})
		return err
	})
	return changed > 0, err
}

// mergeReceipt returns payload with "execution_result" set. ok is false when
// the stored payload names a different workbook. Numbers are preserved
// verbatim (json.Number) so the rest of the payload is not reformatted.
func mergeReceipt(payload []byte, workbookID string, res Result, retryCount int, now time.Time) ([]byte, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, false, fmt.Errorf("workbook: receipt payload: %w", err)
	}
	if wb, _ := doc["workbook_id"].(string); wb != workbookID {
		return nil, false, nil
	}
	doc["execution_result"] = map[string]any{
		"completed": res.Completed, "errors": res.Errors, "total": res.Total, "rows": res.Rows,
		"stopped": res.Stopped, "retry_count": retryCount,
		// datetime.now(timezone.utc).isoformat()
		"recorded_at": now.Format("2006-01-02T15:04:05.000000-07:00"),
	}
	out, err := json.Marshal(doc)
	return out, err == nil, err
}

// Handler adapts Run to the worker's job runner: execute, persist the receipt
// (a failure is logged and never retries already-executed paid calls, like
// handle_run_workbook), and surface only the run error.
func Handler(deps Deps) func(ctx context.Context, c *queue.Claim) error {
	return func(ctx context.Context, c *queue.Claim) error {
		res, err := Run(ctx, deps, c)
		if err != nil {
			return err
		}
		log := deps.Log
		if _, rerr := PersistReceipt(context.WithoutCancel(ctx), deps, c, res); rerr != nil && log != nil {
			log.Error("could not persist workbook result", "err", rerr)
		}
		return nil
	}
}
