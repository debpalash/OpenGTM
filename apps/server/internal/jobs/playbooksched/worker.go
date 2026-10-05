// Package playbooksched is the Go executor for the research_playbook_schedule
// job type, a behavioural port of handle_playbook_schedule and schedule_next in
// apps/api/services/playbooks/scheduler.py.
//
// A research_playbook_schedule job is the recurring "tick" of a playbook that
// runs on an audience: it creates a pending playbook run and its
// research_playbook_run job (still executed by Python, which owns the AI
// engine), then books the next tick. The job type is Python-owned until an
// operator routes it:
//
//	opengtm routes set research_playbook_schedule go      # cut over
//	opengtm routes set research_playbook_schedule python  # roll back
//
// The scheduler's bootstrap_playbook_schedules and the API routes that
// configure playbooks stay in Python.
package playbooksched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// JobType is the queue job type this package executes.
const JobType = "research_playbook_schedule"

// runJobType is the Python-owned job that executes a playbook run.
const runJobType = "research_playbook_run"

// maxMembers is the member cap every scheduled run carries (Python's literal).
const maxMembers = 100

// Interval bounds, in minutes: max(15, min(int(minutes or 60), 10080)).
const (
	defaultMinutes = 60
	minMinutes     = 15
	maxMinutes     = 10080
)

var mirror = jobkit.Mirror{
	Table: "playbook_schedules", KeyCol: "playbook_id", WorkspaceCol: "workspace_id",
	EnabledCol: "enabled", NextCol: "next_run_at",
}

// Worker executes research_playbook_schedule jobs.
type Worker struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// Now is the clock for the next occurrence. Python reads the process
	// clock; tests freeze it.
	Now func() time.Time
}

// NewWorker builds the job handler.
func NewWorker(pool *pgxpool.Pool, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{pool: pool, log: log, Now: func() time.Time { return time.Now().UTC() }}
}

// Register adds the handler to a queue registry. Python registers no failure
// reconciler for this type (a failed tick leaves no domain state to repair;
// the next bootstrap pass rebuilds the occurrence), so neither does Go.
func (w *Worker) Register(r *queue.Registry) { r.Register(JobType, w.Handle) }

type playbook struct {
	enabled      bool
	audienceID   *string
	intervalMins *int64
	version      int32
	prompt       string
	steps        string
	nextRunSet   bool
}

func loadPlaybook(ctx context.Context, tx pgx.Tx, workspaceID, playbookID string) (*playbook, error) {
	var p playbook
	err := tx.QueryRow(ctx, `SELECT enabled, schedule_audience_id, schedule_interval_minutes, version,
			prompt_template, steps::text, next_run_at IS NOT NULL
		FROM research_playbooks WHERE id = $1 AND workspace_id = $2`, playbookID, workspaceID).
		Scan(&p.enabled, &p.audienceID, &p.intervalMins, &p.version, &p.prompt, &p.steps, &p.nextRunSet)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Handle runs one attempt of handle_playbook_schedule.
//
// Python commits the new run, its job and the next occurrence in three
// separate transactions; Go does the whole tick in one tenant transaction
// fenced by the job lease, so a cancelled or reclaimed attempt writes nothing
// and a failure cannot leave a run without its job or its next occurrence.
func (w *Worker) Handle(ctx context.Context, job queue.Job) error {
	payload, err := jobkit.PayloadObject(job.Payload)
	if err != nil {
		return err
	}
	workspaceID, playbookID := payload.Field("workspace_id"), payload.Field("playbook_id")
	if workspaceID == "" || playbookID == "" {
		return errors.New("playbook schedule requires workspace_id and playbook_id")
	}
	return jobkit.WithLease(ctx, w.pool, workspaceID, job, func(tx pgx.Tx) error {
		pb, err := loadPlaybook(ctx, tx, workspaceID, playbookID)
		if err != nil {
			return err
		}
		if pb == nil || !pb.enabled || pb.audienceID == nil || *pb.audienceID == "" {
			return removeSchedule(ctx, tx, workspaceID, playbookID)
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM playbook_runs
			WHERE workspace_id = $1 AND playbook_id = $2 AND status IN ('pending', 'running'))`,
			workspaceID, playbookID).Scan(&active); err != nil {
			return err
		}
		if !active {
			runID, err := createRun(ctx, tx, workspaceID, playbookID, pb)
			if err != nil {
				return err
			}
			if _, err := queue.Enqueue(ctx, tx, runJobType,
				map[string]string{"workspace_id": workspaceID, "run_id": runID},
				queue.EnqueueOptions{FireKey: "playbook:" + runID}); err != nil {
				return err
			}
			w.log.Info("playbook run scheduled", "job_id", job.ID, "playbook_id", playbookID, "run_id", runID)
		}
		_, err = w.scheduleNext(ctx, tx, workspaceID, playbookID, pb)
		return err
	})
}

// removeSchedule ports remove_schedule. Unlike Python it only touches rows
// that belong to the job's own workspace: the mirror and jobs tables have no
// row-level security, and the playbook id comes from the payload.
func removeSchedule(ctx context.Context, tx pgx.Tx, workspaceID, playbookID string) error {
	if err := mirror.DeleteFor(ctx, tx, playbookID, workspaceID); err != nil {
		return err
	}
	_, err := jobkit.CancelPendingFor(ctx, tx, JobType, "playbook_schedule:"+playbookID+":%", workspaceID)
	return err
}

// createRun inserts the pending run the scheduler requests, snapshotting the
// playbook's prompt and steps (`steps or []`).
func createRun(ctx context.Context, tx pgx.Tx, workspaceID, playbookID string, pb *playbook) (string, error) {
	steps := "[]"
	if v, err := jobkit.Decode([]byte(pb.steps)); err == nil && jobkit.Truthy(v) {
		steps = pb.steps
	}
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO playbook_runs
		(id, workspace_id, playbook_id, audience_id, status, prompt_version, prompt_snapshot, steps_snapshot,
		 max_members, attempted, succeeded, failed, requested_by)
		VALUES (gen_random_uuid()::text, $1, $2, $3, 'pending', $4, $5, $6::json, $7, 0, 0, 0, 'scheduler')
		RETURNING id`, workspaceID, playbookID, *pb.audienceID, pb.version, pb.prompt, steps, maxMembers).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create playbook run: %w", err)
	}
	return id, nil
}

// scheduleNext ports schedule_next: cancel pending occurrences, mirror and
// store the next run time, and enqueue the next occurrence once. A playbook is
// "active" when it is enabled, targets an audience and has an interval.
func (w *Worker) scheduleNext(ctx context.Context, tx pgx.Tx, workspaceID, playbookID string, pb *playbook) (*time.Time, error) {
	if _, err := jobkit.CancelPendingFor(ctx, tx, JobType, "playbook_schedule:"+playbookID+":%", workspaceID); err != nil {
		return nil, err
	}
	active := pb.enabled && pb.audienceID != nil && *pb.audienceID != "" && pb.intervalMins != nil && *pb.intervalMins != 0
	var next *time.Time
	if active {
		minutes := jobkit.ClampMinutes(pb.intervalMins, defaultMinutes, minMinutes, maxMinutes)
		n := jobkit.Micros(w.Now().UTC()).Add(time.Duration(minutes) * time.Minute)
		next = &n
	}
	if err := mirror.Upsert(ctx, tx, playbookID, workspaceID, active, next); err != nil {
		return nil, err
	}
	// The ORM compares a naive stored value with the new aware one as
	// different, so any non-null next_run_at is rewritten (bumping updated_at).
	if next != nil || pb.nextRunSet {
		if _, err := tx.Exec(ctx, `UPDATE research_playbooks
			SET next_run_at = $3::timestamptz::timestamp, updated_at = now()
			WHERE id = $1 AND workspace_id = $2`, playbookID, workspaceID, next); err != nil {
			return nil, fmt.Errorf("update playbook next_run_at: %w", err)
		}
	}
	if next != nil {
		fireKey := fmt.Sprintf("playbook_schedule:%s:%s", playbookID, jobkit.IsoFormat(*next))
		payload := struct {
			WorkspaceID string `json:"workspace_id"`
			PlaybookID  string `json:"playbook_id"`
		}{workspaceID, playbookID}
		if _, _, err := queue.EnqueueOnce(ctx, tx, JobType, payload, fireKey, *next); err != nil {
			return nil, err
		}
	}
	return next, nil
}
