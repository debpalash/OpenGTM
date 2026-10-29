package playbooks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// ExecutorFor names the executor of research_playbook_run and research_playbook_schedule jobs:
// always "go" since the plan 15 cutover (GO_EXECUTOR_PLAYBOOKS and the cohort flags are retired).
func ExecutorFor(config.ExecutorSettings, config.GovernanceSettings, string) string { return "go" }

// WireExecutors points the package-level ScheduleExecutor (used by Patch and
// ScheduleNext) at the cohort rule. Call it once at server start.
func WireExecutors(ex config.ExecutorSettings, g config.GovernanceSettings) {
	ScheduleExecutor = func(ws string) string { return ExecutorFor(ex, g, ws) }
}

// ScheduleFireKey is playbook_schedule:<id>:<next_at.isoformat()>. Python's next_at
// is an aware UTC datetime, so the instant renders in UTC with a +00:00 offset
// (microseconds only when non-zero).
func ScheduleFireKey(playbookID string, at time.Time) string {
	return schedulePrefix(playbookID) + pyISOAware(at)
}

// scheduleDelay is timedelta(minutes=max(15, min(minutes, 10080))).
func scheduleDelay(minutes int) time.Duration {
	return time.Duration(max(15, min(minutes, 10080))) * time.Minute
}

func txTenant(ctx context.Context, tx pgx.Tx) (string, error) {
	var ws string
	if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.workspace_id', true), '')`).Scan(&ws); err != nil {
		return "", fmt.Errorf("playbooks: read tenant: %w", err)
	}
	if ws == "" {
		return "", errors.New("playbooks: schedule needs a workspace transaction")
	}
	return ws, nil
}

// RemoveSchedule is remove_schedule: delete the playbook's schedule mirror and
// cancel its pending research_playbook_schedule occurrences. The workspace is
// the transaction's tenant (run inside tenancy.InWorkspace); only that
// workspace's mirror and jobs are touched, and only keys of exactly this
// playbook (never a playbook whose id merely extends it).
func RemoveSchedule(ctx context.Context, tx pgx.Tx, id string) error {
	ws, err := txTenant(ctx, tx)
	if err != nil {
		return err
	}
	q := gen.New(tx)
	if err := q.PbDeleteScheduleMirror(ctx, gen.PbDeleteScheduleMirrorParams{PlaybookID: id, WorkspaceID: ws}); err != nil {
		return fmt.Errorf("playbooks: remove mirror: %w", err)
	}
	if err := q.PbCancelScheduleJobs(ctx, gen.PbCancelScheduleJobsParams{WorkspaceID: ws, FirePrefix: schedulePrefix(id)}); err != nil {
		return fmt.Errorf("playbooks: cancel schedule jobs: %w", err)
	}
	return nil
}

// ScheduleNext is schedule_next. In the caller's workspace transaction: cancel
// the playbook's pending occurrences, compute the next run (now plus the
// interval clamped to 15..10080 minutes, only when the playbook is enabled and
// has a schedule audience and a non-zero interval), upsert the playbook_schedules
// mirror, set the playbook's next_run_at and enqueue one occurrence job
// (single-flight per fire key). The workspace is the transaction's tenant and the
// playbook must exist in it (ErrNotFound otherwise, nothing written). pb.NextRunAt
// is refreshed. The returned time is the UTC next run, nil when inactive.
func ScheduleNext(ctx context.Context, tx pgx.Tx, pb *Playbook, now time.Time) (*time.Time, error) {
	if pb == nil || pb.ID == "" {
		return nil, errors.New("playbooks: schedule needs a playbook")
	}
	ws, err := txTenant(ctx, tx)
	if err != nil {
		return nil, err
	}
	q := gen.New(tx)
	if _, err := q.PbLockPlaybook(ctx, gen.PbLockPlaybookParams{WorkspaceID: ws, ID: pb.ID}); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("playbooks: schedule lock: %w", err)
	}
	if err := q.PbCancelScheduleJobs(ctx, gen.PbCancelScheduleJobsParams{WorkspaceID: ws, FirePrefix: schedulePrefix(pb.ID)}); err != nil {
		return nil, fmt.Errorf("playbooks: cancel schedule jobs: %w", err)
	}
	active := pb.Enabled && pb.ScheduleAudienceID != nil && *pb.ScheduleAudienceID != "" &&
		pb.ScheduleIntervalMinutes != nil && *pb.ScheduleIntervalMinutes != 0
	var next *time.Time
	var nextTS pgtype.Timestamp
	if active {
		n := now.UTC().Truncate(time.Microsecond).Add(scheduleDelay(*pb.ScheduleIntervalMinutes))
		next, nextTS = &n, pgtype.Timestamp{Time: n, Valid: true}
	}
	if err := q.PbUpsertScheduleMirror(ctx, gen.PbUpsertScheduleMirrorParams{
		PlaybookID: pb.ID, WorkspaceID: ws, Enabled: active, NextRunAt: nextTS,
	}); err != nil {
		return nil, fmt.Errorf("playbooks: schedule mirror: %w", err)
	}
	if next != nil || pb.NextRunAt != nil { // SQLAlchemy only writes a changed attribute
		if err := q.PbSetPlaybookNextRun(ctx, gen.PbSetPlaybookNextRunParams{WorkspaceID: ws, ID: pb.ID, NextRunAt: nextTS}); err != nil {
			return nil, fmt.Errorf("playbooks: playbook next run: %w", err)
		}
	}
	pb.NextRunAt = next
	if next == nil {
		return nil, nil
	}
	if err := enqueueSchedule(ctx, tx, ws, pb.ID, *next); err != nil {
		return nil, err
	}
	return next, nil
}

// Deps is what HandleSchedule needs.
type Deps struct {
	Pool *pgxpool.Pool
	// Now is the clock (default time.Now).
	Now func() time.Time
	// RunExecutor picks the executor of the research_playbook_run job (nil: "go", the
	// only executor since the cutover). The schedule job's executor is ScheduleExecutor.
	RunExecutor func(workspaceID string) string
	// Researcher runs a playbook step (HandleRun); nil means no research runner is
	// wired and a claimed run fails permanently.
	Researcher Researcher
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// SchedulerRequestedBy is the requested_by of runs the schedule creates.
const SchedulerRequestedBy = "scheduler"

// scheduledMaxMembers is the member cap of a scheduled run.
const scheduledMaxMembers = 100

// HandleSchedule is handle_playbook_schedule for one research_playbook_schedule
// job. In one workspace transaction: a playbook that is gone, disabled or without
// a schedule audience loses its schedule and the handler returns; otherwise, when
// the playbook has no pending or running run, a scheduler run (max_members 100) is
// inserted and its research_playbook_run job enqueued atomically; then the next
// occurrence is scheduled. The playbook row lock serialises concurrent handlers,
// so two of them create one run.
func HandleSchedule(ctx context.Context, d Deps, workspaceID, playbookID string) error {
	if workspaceID == "" || playbookID == "" {
		return errors.New("playbook schedule requires workspace_id and playbook_id")
	}
	now := d.now()
	return tenancy.InWorkspace(ctx, d.Pool, workspaceID, func(tx pgx.Tx) error {
		if ws, err := txTenant(ctx, tx); err != nil {
			return err
		} else if ws != workspaceID {
			return fmt.Errorf("playbooks: workspace %q does not match transaction tenant %q", workspaceID, ws)
		}
		q := gen.New(tx)
		row, err := q.PbLockPlaybook(ctx, gen.PbLockPlaybookParams{WorkspaceID: workspaceID, ID: playbookID})
		if errors.Is(err, pgx.ErrNoRows) {
			return RemoveSchedule(ctx, tx, playbookID)
		}
		if err != nil {
			return fmt.Errorf("playbooks: schedule load: %w", err)
		}
		pb, err := playbookFrom(row)
		if err != nil {
			return err
		}
		if !pb.Enabled || pb.ScheduleAudienceID == nil || *pb.ScheduleAudienceID == "" {
			return RemoveSchedule(ctx, tx, playbookID)
		}
		active, err := q.PbActiveRunExists(ctx, gen.PbActiveRunExistsParams{WorkspaceID: workspaceID, PlaybookID: playbookID})
		if err != nil {
			return fmt.Errorf("playbooks: schedule active run: %w", err)
		}
		if !active {
			run, err := StartRun(ctx, tx, workspaceID, pb, *pb.ScheduleAudienceID, scheduledMaxMembers, SchedulerRequestedBy)
			if err != nil {
				return err
			}
			executor := "go"
			if d.RunExecutor != nil {
				executor = d.RunExecutor(workspaceID)
			}
			if err := EnqueueRun(ctx, tx, workspaceID, run.ID, executor); err != nil {
				return err
			}
		}
		_, err = ScheduleNext(ctx, tx, &pb, now)
		return err
	})
}
