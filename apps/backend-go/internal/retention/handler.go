package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// decodePayload reads workspace_id and run_id like handle_retention_enforce: a falsy
// workspace_id is the Python ValueError, run_id is optional. A payload that is not a JSON
// object reads as empty.
func decodePayload(raw []byte) (ws, runID string, err error) {
	var p map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if json.Unmarshal(raw, &p) != nil {
			p = nil
		}
	}
	ws, runID = payloadString(p["workspace_id"]), payloadString(p["run_id"])
	if ws == "" {
		return "", "", errors.New("retention enforcement requires workspace_id")
	}
	return ws, runID, nil
}

// Handler is the retention_enforce job handler. The worker's heartbeat and cancel
// watcher own the lease (a lost lease cancels ctx, which stops the purge before its next
// batch) and its panic guard and failure hook own failure bookkeeping, so the handler only
// decodes the payload, pins it to the job row's workspace and runs Enforce.
func Handler(d Deps) func(ctx context.Context, c *queue.Claim) error {
	return func(ctx context.Context, c *queue.Claim) error {
		ws, runID, err := decodePayload(c.Payload)
		if err != nil {
			return err
		}
		if ws != c.WorkspaceID {
			return &queue.PermanentError{Err: fmt.Errorf("retention job %d payload workspace differs from the job's", c.ID)}
		}
		return Enforce(ctx, d, ws, runID)
	}
}

// FailureHook is reconcile_retention_job_failure for the queue: after a failed attempt was
// persisted it settles the run, with willRetry from the job row (retry_count < max_retries).
// The payload workspace must equal the job row's workspace; otherwise (or for a malformed
// payload) nothing is touched.
func FailureHook(d Deps) queue.FailureHook {
	return func(ctx context.Context, n queue.FailureNotice) error {
		ws, _, err := decodePayload(n.Payload)
		if err != nil || ws != n.WorkspaceID {
			slog.Error("retention failure hook: payload does not match the job; ignored", "job_id", n.JobID)
			return nil
		}
		return Reconcile(ctx, d, n.JobID, n.Payload, n.Reason, n.WillRetry)
	}
}

// RegisterFailureHook wires FailureHook as the failure hook of retention_enforce.
func RegisterFailureHook(d Deps) { queue.RegisterFailureHook(JobType, FailureHook(d)) }
