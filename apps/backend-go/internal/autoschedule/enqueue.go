package autoschedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Trigger is the slice of a triggers row the scheduler needs.
type Trigger struct {
	ID          string
	WorkspaceID string
	Enabled     bool
	Type        string
	// Interval is trigger_config["interval"] (daily when absent or not a string).
	Interval string
	// Anchor is schedule_anchor; nil means "now" at computation time.
	Anchor *time.Time
}

// LoadTrigger reads one trigger inside tenancy.InWorkspace. ok is false when the
// rule is missing in that workspace.
func LoadTrigger(ctx context.Context, tx pgx.Tx, id, ws string) (t Trigger, ok bool, err error) {
	r, err := gen.New(tx).AutoScheduleTrigger(ctx, gen.AutoScheduleTriggerParams{ID: id, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return Trigger{}, false, nil
	}
	if err != nil {
		return Trigger{}, false, fmt.Errorf("autoschedule: load trigger: %w", err)
	}
	t = Trigger{ID: r.ID, WorkspaceID: r.WorkspaceID, Enabled: r.Enabled, Type: r.TriggerType, Interval: intervalOf(r.TriggerConfig)}
	if r.ScheduleAnchor.Valid {
		a := r.ScheduleAnchor.Time.UTC()
		t.Anchor = &a
	}
	return t, true, nil
}

func intervalOf(cfg string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(cfg), &m) != nil {
		return "daily"
	}
	var s string
	if raw, ok := m["interval"]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return "daily"
}

// next is compute_next_run(schedule_anchor or now). With no anchor Python reads
// the clock a second time inside compute_next_run, so the result is one
// interval ahead (never "due now", which would spin the schedule). Absolute
// time and UTC fire keys: the Postgres session TimeZone is never read (R-tz; a
// UTC session is a documented canary precondition so Python's keys match).
func (t Trigger) next(now time.Time) time.Time {
	if t.Anchor == nil {
		return now.UTC().Add(IntervalDelta(t.Interval))
	}
	return NextRun(*t.Anchor, t.Interval, now)
}

type schedulePayload struct {
	TriggerID   string `json:"trigger_id"`
	WorkspaceID string `json:"workspace_id"`
	FireSource  string `json:"fire_source"`
	FireKey     string `json:"fire_key"`
}

// EnqueueScheduleIfAbsent is _enqueue_schedule_if_absent: single-flight insert
// of a trigger_eval keyed sched:<id>:<next isoformat> (advisory lock text =
// the fire key, as enqueue_job_once), then triggers.next_run_at and the mirror
// in the same transaction. false when an active job already holds the key (no
// trigger or mirror write, as in Python). tx must be inside tenancy.InWorkspace.
func EnqueueScheduleIfAbsent(ctx context.Context, tx pgx.Tx, t Trigger, next time.Time, executor string) (bool, error) {
	if executor != "go" {
		return false, fmt.Errorf("autoschedule: executor %q must be go (the only executor since the plan 15 cutover)", executor)
	}
	next = next.UTC()
	key := FireKey(t.ID, next)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(schedulePayload{t.ID, t.WorkspaceID, "schedule", key}); err != nil {
		return false, fmt.Errorf("autoschedule: encode payload: %w", err)
	}
	q := gen.New(tx)
	if err := q.LockJobFireKey(ctx, key); err != nil {
		return false, fmt.Errorf("autoschedule: lock: %w", err)
	}
	if _, err := q.ActiveJobByFireKey(ctx, key); err == nil {
		return false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("autoschedule: look up: %w", err)
	}
	_, err := q.InsertScheduleJob(ctx, gen.InsertScheduleJobParams{
		Executor: executor, Payload: bytes.TrimRight(buf.Bytes(), "\n"), WorkspaceID: t.WorkspaceID, FireKey: key,
		NextRunAt: tstz(next),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // lost the unique-index race
	}
	if err != nil {
		return false, fmt.Errorf("autoschedule: enqueue: %w", err)
	}
	if err := q.SetTriggerNextRun(ctx, gen.SetTriggerNextRunParams{ID: t.ID, WorkspaceID: t.WorkspaceID, NextRunAt: tstz(next)}); err != nil {
		return false, fmt.Errorf("autoschedule: set next_run_at: %w", err)
	}
	if err := MirrorSet(ctx, tx, t.ID, t.WorkspaceID, &next, true); err != nil {
		return false, err
	}
	return true, nil
}
