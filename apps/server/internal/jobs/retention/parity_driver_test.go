package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit/paritytest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// These two tests are the Go half of the Python/Go parity harness
// (tests/test_retention_go_parity_pg.py). They are inert unless the harness
// sets the OPENGTM_PARITY_* variables, and they only drive the code under
// test and write its observable results; the harness compares them with the
// Python results. Run the whole thing with:
//
//	TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
//	  uv run pytest tests/test_retention_go_parity_pg.py

// TestParityDriver replays scenarios.json against a database the harness has
// already seeded, calling the Go handler exactly as the Python runner calls
// its own, with the clock frozen to the scenario instant.
func TestParityDriver(t *testing.T) {
	paritytest.Replay(t, func(ctx context.Context, env *paritytest.Env, step paritytest.Step, job queue.Job) error {
		w := NewWorker(env.App, nil)
		w.Now = func() time.Time { return env.Spec.Now }
		w.BatchSize = 7 // many batches: the Python side deletes each table in one statement
		switch step.Op {
		case "enforce":
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

type pureValue struct {
	T string `json:"t"`
	V any    `json:"v"`
}

type pureCutoff struct {
	Values []pureValue `json:"values"`
	Error  *string     `json:"error"`
}

type pureNormalize struct {
	OK    map[string]int64 `json:"ok,omitempty"`
	Order []string         `json:"order,omitempty"`
	Error *string          `json:"error,omitempty"`
}

// isoformat is datetime.isoformat() for a naive datetime.
func isoformat(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

// TestParityPure evaluates normalized_days and the cutoff arithmetic over the
// shared corpus (tests/retention_parity/pure_cases.json).
func TestParityPure(t *testing.T) {
	casesPath, out := os.Getenv("OPENGTM_PARITY_PURE"), os.Getenv("OPENGTM_PARITY_OUT")
	if casesPath == "" || out == "" {
		t.Skip("OPENGTM_PARITY_PURE not set; this test is driven by tests/test_retention_go_parity_pg.py")
	}
	raw, err := os.ReadFile(casesPath)
	if err != nil {
		t.Fatal(err)
	}
	var cases struct {
		Normalize []json.RawMessage `json:"normalize"`
		Cutoff    []struct {
			Snapshot json.RawMessage `json:"snapshot"`
			Category string          `json:"category"`
			Now      string          `json:"now"`
		} `json:"cutoff"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}

	result := struct {
		Normalize []pureNormalize `json:"normalize"`
		Cutoff    []pureCutoff    `json:"cutoff"`
	}{Normalize: []pureNormalize{}, Cutoff: []pureCutoff{}}

	for _, c := range cases.Normalize {
		days, err := normalizedDays(c)
		if err != nil {
			msg := err.Error()
			result.Normalize = append(result.Normalize, pureNormalize{Error: &msg})
			continue
		}
		result.Normalize = append(result.Normalize, pureNormalize{OK: days, Order: Categories})
	}
	for _, c := range cases.Cutoff {
		now, err := time.Parse("2006-01-02T15:04:05", c.Now)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := jobkit.Decode(c.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		r := pureCutoff{Values: []pureValue{}}
		for _, tg := range targets {
			cutoff, err := cutoffFor(snapshot, tg.category, now)
			if err != nil {
				msg := err.Error()
				r.Error = &msg
				break
			}
			arg, _ := tg.cutoffArg(cutoff)
			switch v := arg.(type) {
			case time.Time:
				r.Values = append(r.Values, pureValue{"timestamp", isoformat(v)})
			case string:
				r.Values = append(r.Values, pureValue{"date", v})
			case float64:
				r.Values = append(r.Values, pureValue{"epoch", v})
			}
		}
		result.Cutoff = append(result.Cutoff, r)
	}
	paritytest.WriteJSON(t, out, result)
}
