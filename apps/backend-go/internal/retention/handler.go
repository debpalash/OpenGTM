package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
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

var scheduledFireKey = regexp.MustCompile(`^retention:(.+):[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// verifyScheduled admits a job whose jobs.workspace_id is empty (the shape Python's
// enqueue_job_once and SchedulePolicy write for the scheduled occurrence). Nothing in the
// payload is trusted on its own: the job's own fire_key must be exactly
// retention:<ws>:<date>, name the payload's workspace, the payload must carry no run_id
// (a requested run always has a job workspace), and the workspace's own policy, read in
// that tenant, must exist and be enabled.
func verifyScheduled(ctx context.Context, d Deps, jobID int64, ws, runID string) error {
	if runID != "" {
		return fmt.Errorf("retention job %d has a run_id but no workspace", jobID)
	}
	var fk *string
	if err := d.Pool.QueryRow(ctx, `SELECT fire_key FROM jobs WHERE id = $1 AND type = 'retention_enforce'`, jobID).Scan(&fk); err != nil {
		return fmt.Errorf("retention job %d: read fire key: %w", jobID, err)
	}
	if fk == nil {
		return fmt.Errorf("retention job %d has no workspace and no fire key", jobID)
	}
	m := scheduledFireKey.FindStringSubmatch(*fk)
	if m == nil || m[1] != ws {
		return fmt.Errorf("retention job %d fire key does not name the payload workspace", jobID)
	}
	enabled := false
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		p, err := gen.New(tx).RetGetPolicy(ctx, ws)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		enabled = err == nil && p.Enabled
		return err
	})
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("retention job %d: workspace has no enabled retention policy", jobID)
	}
	return nil
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
		if c.WorkspaceID != "" {
			if ws != c.WorkspaceID {
				return &queue.PermanentError{Err: fmt.Errorf("retention job %d payload workspace differs from the job's", c.ID)}
			}
		} else if err := verifyScheduled(ctx, d, c.ID, ws, runID); err != nil {
			return &queue.PermanentError{Err: err}
		}
		return Enforce(ctx, d, ws, runID)
	}
}

// FailureHook is reconcile_retention_job_failure for the queue: after a failed attempt was
// persisted it settles the run, with willRetry from the job row (retry_count < max_retries).
// The payload workspace must equal the job row's workspace when the row has one (scheduler
// jobs have none and the payload names the tenant); otherwise (or for a malformed
// payload) nothing is touched.
func FailureHook(d Deps) queue.FailureHook {
	return func(ctx context.Context, n queue.FailureNotice) error {
		ws, runID, err := decodePayload(n.Payload)
		if err == nil && n.WorkspaceID == "" {
			err = verifyScheduled(ctx, d, n.JobID, ws, runID)
		} else if err == nil && ws != n.WorkspaceID {
			err = errors.New("payload workspace differs from the job's")
		}
		if err != nil {
			slog.Error("retention failure hook: payload does not match the job; ignored", "job_id", n.JobID)
			return nil
		}
		return Reconcile(ctx, d, n.JobID, n.Payload, n.Reason, n.WillRetry)
	}
}

// RegisterFailureHook wires FailureHook as the failure hook of retention_enforce.
func RegisterFailureHook(d Deps) { queue.RegisterFailureHook(JobType, FailureHook(d)) }
