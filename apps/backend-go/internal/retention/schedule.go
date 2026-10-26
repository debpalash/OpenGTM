package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// JobType is the queue job type of a scheduled or requested enforcement.
const JobType = "retention_enforce"

// ExecutorFor is go_executor.executor_for("retention_enforce", ws): "go" only when
// GO_EXECUTOR_ENABLED, GO_EXECUTOR_RETENTION and the workspace is in
// GO_EXECUTOR_WORKSPACES. An empty workspace is never in the cohort.
func ExecutorFor(ex config.ExecutorSettings, g config.GovernanceSettings, ws string) string {
	if ws != "" && ex.Enabled && g.GoExecutorRetention && ex.Workspaces[ws] {
		return "go"
	}
	return "python"
}

// FireKey is the scheduled occurrence key retention:<ws>:<YYYY-MM-DD> (the date of
// the next run in UTC: Python's now is UTC and so is its next_at.date()).
func FireKey(ws string, next time.Time) string {
	return "retention:" + ws + ":" + next.UTC().Format("2006-01-02")
}

// SchedulePolicy is schedule_policy. In the caller's transaction (inside
// tenancy.InWorkspace; call right after UpsertPolicy): cancel the workspace's
// pending retention_enforce jobs whose fire key starts retention:<ws>:, compute the
// next run (now+1 day only when the policy is enabled and not on legal hold), set the
// policy's next_run_at and mirror it into retention_schedules, and enqueue one job
// due at that time (single-flight per fire key). executorFor picks "go" or "python"
// per workspace (nil: python). The returned time is the UTC next run, nil when no
// run is scheduled; p.NextRunAt/UpdatedAt are refreshed from the stored row.
//
// Unlike queue.EnqueueOnce the job's next_run_at is the next occurrence, not now:
// an immediate job here would purge data a day early.
func SchedulePolicy(ctx context.Context, tx pgx.Tx, p *Policy, now time.Time, executorFor func(ws string) string) (*time.Time, error) {
	if p == nil || p.WorkspaceID == "" {
		return nil, errors.New("retention: schedule needs a policy with a workspace")
	}
	ws := p.WorkspaceID
	q := gen.New(tx)
	if _, err := q.RetCancelPendingJobs(ctx, "retention:"+ws+":"); err != nil {
		return nil, fmt.Errorf("retention: cancel pending: %w", err)
	}
	var next *time.Time
	if p.Enabled && !p.LegalHold {
		n := now.UTC().Truncate(time.Microsecond).Add(24 * time.Hour)
		next = &n
	}
	var nextTS pgtype.Timestamptz
	if next != nil {
		nextTS = pgtype.Timestamptz{Time: *next, Valid: true}
	}
	if err := q.RetUpsertMirror(ctx, gen.RetUpsertMirrorParams{WorkspaceID: ws, Enabled: next != nil, NextRunAt: nextTS}); err != nil {
		return nil, fmt.Errorf("retention: mirror: %w", err)
	}
	if next != nil || p.NextRunAt != nil { // SQLAlchemy only writes a changed attribute
		row, err := q.RetSetPolicyNextRun(ctx, gen.RetSetPolicyNextRunParams{WorkspaceID: ws, NextRunAt: nextTS})
		if err != nil {
			return nil, fmt.Errorf("retention: policy next run: %w", err)
		}
		p.NextRunAt, p.UpdatedAt = nil, row.UpdatedAt.Time
		if row.NextRunAt.Valid {
			t := row.NextRunAt.Time
			p.NextRunAt = &t
		}
	}
	if next == nil {
		return nil, nil
	}
	executor := "python"
	if executorFor != nil {
		executor = executorFor(ws)
	}
	if executor != "go" && executor != "python" {
		return nil, fmt.Errorf("retention: executor %q must be go or python", executor)
	}
	key := FireKey(ws, *next)
	if err := q.LockJobFireKey(ctx, key); err != nil {
		return nil, fmt.Errorf("retention: lock: %w", err)
	}
	if _, err := q.ActiveJobByFireKey(ctx, key); err == nil {
		return next, nil // an active occurrence already holds the key
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("retention: look up job: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		WorkspaceID string `json:"workspace_id"`
		FireKey     string `json:"fire_key"`
	}{ws, key}); err != nil {
		return nil, err
	}
	if _, err := q.RetInsertScheduledJob(ctx, gen.RetInsertScheduledJobParams{
		Executor: executor, Payload: bytes.TrimSpace(buf.Bytes()), FireKey: key, NextRunAt: nextTS,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) { // ErrNoRows: lost the unique-index race
		return nil, fmt.Errorf("retention: enqueue: %w", err)
	}
	return next, nil
}
