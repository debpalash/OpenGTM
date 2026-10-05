package playbooksched

import (
	"context"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit/paritytest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// TestParityDriver is the Go half of tests/test_playbook_schedule_go_parity_pg.py:
// it replays scenarios.json against a database the harness has seeded, with the
// clock frozen to the scenario instant, and records each step's outcome.
func TestParityDriver(t *testing.T) {
	paritytest.Replay(t, func(ctx context.Context, env *paritytest.Env, step paritytest.Step, job queue.Job) error {
		if step.Op != "tick" {
			t.Fatalf("unknown op %q", step.Op)
		}
		w := NewWorker(env.App, nil)
		w.Now = func() time.Time { return env.Spec.Now }
		return w.Handle(ctx, job)
	})
}
