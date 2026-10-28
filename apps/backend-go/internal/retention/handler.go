package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

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

// errSkipScheduled marks a scheduled job with nothing to enforce (no enabled policy): the
// job completes without a run.
var errSkipScheduled = errors.New("retention: no enabled policy; scheduled job skipped")

// verifyScheduled checks a scheduled occurrence of the claim's workspace ws (its fire_key is
// retention:<ws>:<date>); a manual run's job (no fire key, or another shape) needs no check. A
// scheduled key must name ws. A run_id in the payload is the reconcile step's link after a failed
// attempt: it must name an unfinished (pending, running or failed: Enforce marks a run failed itself
// and Python re-runs anything not completed or cancelled) scheduler-requested run in that workspace,
// read inside that tenant. Without a run_id the workspace's policy must exist and be enabled, else
// errSkipScheduled.
func verifyScheduled(ctx context.Context, d Deps, jobID int64, ws, runID string) error {
	var fk *string
	if err := d.Pool.QueryRow(ctx, `SELECT fire_key FROM jobs WHERE id = $1 AND type = 'retention_enforce'`, jobID).Scan(&fk); err != nil {
		return fmt.Errorf("retention job %d: read fire key: %w", jobID, err)
	}
	if fk == nil || !strings.HasPrefix(*fk, "retention:") {
		return nil // a manual enforcement run
	}
	m := scheduledFireKey.FindStringSubmatch(*fk)
	if m == nil || m[1] != ws {
		return fmt.Errorf("retention job %d fire key does not name the job's workspace", jobID)
	}
	var verr error
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if runID != "" {
			run, err := q.RetRunGet(ctx, gen.RetRunGetParams{ID: runID, WorkspaceID: ws})
			if errors.Is(err, pgx.ErrNoRows) {
				verr = fmt.Errorf("retention job %d: run_id names no run in the workspace", jobID)
				return nil
			}
			if err != nil {
				return err
			}
			if run.RequestedBy.String != "scheduler" || (run.Status != "pending" && run.Status != "running" && run.Status != "failed") {
				verr = fmt.Errorf("retention job %d: run_id is not an unfinished scheduler run", jobID)
			}
			return nil
		}
		p, err := q.RetGetPolicy(ctx, ws)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !p.Enabled) {
			verr = errSkipScheduled
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	return verr
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
		// The tenant is the claim's only (final security Low 1): a job without jobs.workspace_id is refused.
		if c.WorkspaceID == "" {
			return &queue.PermanentError{Err: fmt.Errorf("retention job %d has no workspace", c.ID)}
		}
		if ws != c.WorkspaceID {
			return &queue.PermanentError{Err: fmt.Errorf("retention job %d payload workspace differs from the job's", c.ID)}
		}
		if err := verifyScheduled(ctx, d, c.ID, ws, runID); errors.Is(err, errSkipScheduled) {
			return nil
		} else if err != nil {
			return &queue.PermanentError{Err: err}
		}
		return Enforce(ctx, d, ws, runID)
	}
}

// FailureHook is reconcile_retention_job_failure for the queue: after a failed attempt was
// persisted it settles the run, with willRetry from the job row (retry_count < max_retries).
// The payload workspace must equal the job row's workspace, which must be set; otherwise (or
// for a malformed payload) nothing is touched.
func FailureHook(d Deps) queue.FailureHook {
	return func(ctx context.Context, n queue.FailureNotice) error {
		ws, _, err := decodePayload(n.Payload)
		if err == nil && (n.WorkspaceID == "" || ws != n.WorkspaceID) {
			err = errors.New("payload workspace differs from the job's (or the job has none)")
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
