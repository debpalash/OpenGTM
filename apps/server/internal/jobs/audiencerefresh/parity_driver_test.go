package audiencerefresh

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit/paritytest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// TestParityDriver is the Go half of tests/test_audience_refresh_go_parity_pg.py:
// it replays scenarios.json against a database the harness has seeded, with the
// clock frozen to the scenario instant, and records each step's outcome.
func TestParityDriver(t *testing.T) {
	automations := false
	paritytest.Replay(t, func(ctx context.Context, env *paritytest.Env, step paritytest.Step, job queue.Job) error {
		pageSize := 0
		_ = json.Unmarshal(env.Spec.Field("page_size"), &pageSize)
		w := NewWorker(env.App, nil, Options{AutomationsEnabled: automations, PageSize: pageSize})
		w.Now = func() time.Time { return env.Spec.Now }
		switch step.Op {
		case "config":
			automations = step.Bool("automations")
			return nil
		case "refresh":
			return w.Handle(ctx, job)
		case "reconcile":
			message := step.String("error")
			if n := step.Int("error_repeat"); n > 0 {
				message = strings.Repeat(message, n)
			}
			return w.Reconcile(ctx, job, message, step.Bool("will_retry"))
		}
		t.Fatalf("unknown op %q", step.Op)
		return nil
	})
}
