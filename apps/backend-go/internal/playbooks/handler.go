package playbooks

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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
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
// guard and failure hook own the lease and failure bookkeeping. When no Researcher is wired
// the run fails permanently (there is no Python worker to hand it to since the cutover).
func RunHandler(d Deps, r Researcher) func(ctx context.Context, c *queue.Claim) error {
	d.Researcher = r
	return func(ctx context.Context, c *queue.Claim) error {
		f := payloadFields(c.Payload, "workspace_id", "run_id")
		if f[0] == "" || f[1] == "" {
			return errors.New(errRunPayload)
		}
		if c.WorkspaceID == "" || f[0] != c.WorkspaceID { // a run job always carries its workspace
			return foreign(c)
		}
		err := HandleRun(ctx, d, f[0], f[1])
		if errors.Is(err, provcall.ErrUnavailable) {
			// No research runner is wired (there is no Python worker to hand the run to since the cutover):
			// a retry cannot succeed.
			return &queue.PermanentError{Err: fmt.Errorf("playbooks: research runner unavailable: %w", err)}
		}
		return err
	}
}

// verifyScheduleJob checks a research_playbook_schedule job of the claim's workspace. The job's own fire_key must be exactly
// playbook_schedule:<playbook id>:<iso time> for the payload's playbook, and the playbook
// must exist in the payload's workspace (read in that tenant, so a forged workspace sees
// nothing) with an active schedule; anything else is a permanent failure.
func verifyScheduleJob(ctx context.Context, d Deps, jobID int64, ws, playbookID string) error {
	var fk *string
	if err := d.Pool.QueryRow(ctx, `SELECT fire_key FROM jobs WHERE id = $1 AND type = 'research_playbook_schedule'`, jobID).Scan(&fk); err != nil {
		return fmt.Errorf("playbook schedule job %d: read fire key: %w", jobID, err)
	}
	prefix := schedulePrefix(playbookID)
	if fk == nil || !strings.HasPrefix(*fk, prefix) || !scheduleTail.MatchString((*fk)[len(prefix):]) {
		return fmt.Errorf("playbook schedule job %d fire key does not name the payload playbook", jobID)
	}
	found := false
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		_, err := gen.New(tx).PbGetPlaybook(ctx, gen.PbGetPlaybookParams{WorkspaceID: ws, ID: playbookID})
		found = err == nil
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if !found {
		// Forged workspace or a deleted playbook (its pending jobs are cancelled on delete).
		return fmt.Errorf("playbook schedule job %d: playbook not found in the payload workspace", jobID)
	}
	// An inactive playbook is handled by HandleSchedule exactly as Python (schedule removed).
	return nil
}

var scheduleTail = regexp.MustCompile(`^(bootstrap|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?([+-][0-9]{2}:[0-9]{2})?)$`)

// ScheduleHandler is the research_playbook_schedule job handler.
func ScheduleHandler(d Deps) func(ctx context.Context, c *queue.Claim) error {
	return func(ctx context.Context, c *queue.Claim) error {
		f := payloadFields(c.Payload, "workspace_id", "playbook_id")
		if f[0] == "" || f[1] == "" {
			return errors.New("playbook schedule requires workspace_id and playbook_id")
		}
		// The tenant is the claim's only (final security Low 1): a job without jobs.workspace_id is refused.
		if c.WorkspaceID == "" || f[0] != c.WorkspaceID {
			return foreign(c)
		}
		if err := verifyScheduleJob(ctx, d, c.ID, f[0], f[1]); err != nil {
			return &queue.PermanentError{Err: err}
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
		if f[0] == "" || f[1] == "" || n.WorkspaceID == "" || f[0] != n.WorkspaceID {
			slog.Error("playbook failure hook: payload does not match the job; ignored", "job_id", n.JobID)
			return nil
		}
		return ReconcileFailure(ctx, d, n.JobID, n.Payload, n.Reason, n.WillRetry)
	}
}

// RegisterFailureHook wires FailureHook as the failure hook of research_playbook_run.
func RegisterFailureHook(d Deps) { queue.RegisterFailureHook(RunJobType, FailureHook(d)) }
