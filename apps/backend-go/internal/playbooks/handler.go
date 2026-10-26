package playbooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// Queue job types of the playbook executor.
const (
	RunJobType      = "research_playbook_run"
	ScheduleJobType = "research_playbook_schedule"
)

// payloadFields reads the payload keys with Python's `payload.get(k)` truthiness (no
// payload, a non-object or a missing key reads as empty).
func payloadFields(raw []byte, keys ...string) []string {
	var p map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&p) != nil {
		p = nil
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = pyStrip(valueText(p[k]))
	}
	return out
}

func foreign(c *queue.Claim) error {
	return &queue.PermanentError{Err: fmt.Errorf("playbook job %d payload workspace differs from the job's", c.ID)}
}

// RunHandler is the research_playbook_run job handler. The worker's heartbeat, panic
// guard and failure hook own the lease and failure bookkeeping. When the Researcher (the
// Python worker) is unavailable the job goes back to Python with no retry spent.
func RunHandler(d Deps, r Researcher) func(ctx context.Context, c *queue.Claim) error {
	d.Researcher = r
	return func(ctx context.Context, c *queue.Claim) error {
		f := payloadFields(c.Payload, "workspace_id", "run_id")
		if f[0] == "" || f[1] == "" {
			return errors.New(errRunPayload)
		}
		if c.WorkspaceID != "" && f[0] != c.WorkspaceID {
			return foreign(c)
		}
		err := HandleRun(ctx, d, f[0], f[1])
		if !errors.Is(err, pyworker.ErrUnavailable) {
			return err
		}
		ok, herr := queue.HandBack(ctx, d.Pool, c, "python worker unavailable")
		if herr != nil {
			return herr
		}
		if !ok {
			return workbook.ErrLeaseLost
		}
		return fmt.Errorf("%w: python worker unavailable", workbook.ErrHandedBack)
	}
}

// ScheduleHandler is the research_playbook_schedule job handler.
func ScheduleHandler(d Deps) func(ctx context.Context, c *queue.Claim) error {
	return func(ctx context.Context, c *queue.Claim) error {
		f := payloadFields(c.Payload, "workspace_id", "playbook_id")
		if f[0] == "" || f[1] == "" {
			return errors.New("playbook schedule requires workspace_id and playbook_id")
		}
		if c.WorkspaceID != "" && f[0] != c.WorkspaceID {
			return foreign(c)
		}
		return HandleSchedule(ctx, d, f[0], f[1])
	}
}

// FailureHook is reconcile_playbook_job_failure for the queue, with willRetry from the job
// row. The payload workspace must equal the job row's workspace; a mismatch or malformed
// payload touches nothing (ReconcileFailure also scopes every run lookup to the workspace).
func FailureHook(d Deps) queue.FailureHook {
	return func(ctx context.Context, n queue.FailureNotice) error {
		f := payloadFields(n.Payload, "workspace_id", "run_id")
		if f[0] == "" || f[1] == "" || (n.WorkspaceID != "" && f[0] != n.WorkspaceID) {
			slog.Error("playbook failure hook: payload does not match the job; ignored", "job_id", n.JobID)
			return nil
		}
		return ReconcileFailure(ctx, d, n.JobID, n.Payload, n.Reason, n.WillRetry)
	}
}

// RegisterFailureHook wires FailureHook as the failure hook of research_playbook_run.
func RegisterFailureHook(d Deps) { queue.RegisterFailureHook(RunJobType, FailureHook(d)) }
