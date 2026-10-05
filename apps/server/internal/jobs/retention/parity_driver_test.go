package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
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

type parityStep struct {
	Op          string `json:"op"`
	Job         int64  `json:"job"`
	Error       string `json:"error"`
	ErrorRepeat int    `json:"error_repeat"`
	WillRetry   bool   `json:"will_retry"`
}

type parityResult struct {
	Job   int64   `json:"job"`
	Op    string  `json:"op"`
	Error *string `json:"error"`
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestParityDriver replays scenarios.json against a database the harness has
// already seeded, calling the Go handler exactly as the Python runner calls
// its own, with the clock frozen to the scenario instant.
func TestParityDriver(t *testing.T) {
	rawURL, out := os.Getenv("OPENGTM_PARITY_DATABASE_URL"), os.Getenv("OPENGTM_PARITY_OUT")
	scenarios := os.Getenv("OPENGTM_PARITY_SCENARIOS")
	if rawURL == "" || out == "" || scenarios == "" {
		t.Skip("OPENGTM_PARITY_* not set; this test is driven by tests/test_retention_go_parity_pg.py")
	}
	ownerURL, err := config.NormalizeDatabaseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		FrozenNow string       `json:"frozen_now"`
		Steps     []parityStep `json:"steps"`
	}
	raw, err := os.ReadFile(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339, spec.FrozenNow)
	if err != nil {
		t.Fatal(err)
	}

	owner := dbtest.Pool(t, ownerURL, 2)
	app := dbtest.Pool(t, dbtest.AppURL(t, ownerURL), 4)
	w := NewWorker(app, nil)
	w.Now = func() time.Time { return now }
	w.BatchSize = 7 // many batches: the Python side deletes each table in one statement

	ctx := context.Background()
	results := make([]parityResult, 0, len(spec.Steps))
	for _, s := range spec.Steps {
		job := queue.Job{ID: s.Job, Type: JobType}
		var workerID *string
		var lockedAt *time.Time
		var ws *string
		if err := owner.QueryRow(ctx, `SELECT payload::text, workspace_id, coalesce(retry_count, 0), worker_id, locked_at
			FROM jobs WHERE id = $1`, s.Job).Scan(&job.Payload, &ws, &job.RetryCount, &workerID, &lockedAt); err != nil {
			t.Fatalf("job %d: %v", s.Job, err)
		}
		if ws != nil {
			job.WorkspaceID = *ws
		}
		if workerID != nil && lockedAt != nil {
			job.Lease = queue.Lease{WorkerID: *workerID, LockedAt: *lockedAt}
		}
		var stepErr error
		switch s.Op {
		case "enforce":
			stepErr = w.Handle(ctx, job)
		case "reconcile":
			message := s.Error
			if s.ErrorRepeat > 0 {
				message = strings.Repeat(message, s.ErrorRepeat)
			}
			stepErr = w.Reconcile(ctx, job, message, s.WillRetry)
		default:
			t.Fatalf("unknown op %q", s.Op)
		}
		r := parityResult{Job: s.Job, Op: s.Op}
		if stepErr != nil {
			msg := stepErr.Error()
			r.Error = &msg
		}
		results = append(results, r)
	}
	writeJSON(t, out, results)
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
		snapshot, err := decodeValue(c.Snapshot)
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
	writeJSON(t, out, result)
}
