// Package paritytest is the Go half of the Python/Go job parity harness (the
// Python half is tests/parity_harness). A package's TestParityDriver calls
// Replay: it replays a scenarios.json against a database the harness has
// already seeded, handing every step to the Go executor exactly as the Python
// runner hands it to the Python handler, and writes each step's outcome for
// the harness to compare. It is inert unless the harness sets the
// OPENGTM_PARITY_* variables, so it never affects a normal `go test`.
//
// Only test code imports this package.
package paritytest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit/jobtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// Environment variables set by tests/parity_harness/pg.py.
const (
	EnvDatabase  = "OPENGTM_PARITY_DATABASE_URL"
	EnvScenarios = "OPENGTM_PARITY_SCENARIOS"
	EnvOut       = "OPENGTM_PARITY_OUT"
)

// Step is one scenario step. Op and Job are common to every job type; the
// remaining fields are read through the typed accessors.
type Step struct {
	Op   string
	Job  int64
	Note string
	raw  map[string]json.RawMessage
}

// String returns a string field ("" when absent).
func (s Step) String(key string) string {
	var v string
	if raw, ok := s.raw[key]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

// Int returns an integer field (0 when absent).
func (s Step) Int(key string) int {
	var v int
	if raw, ok := s.raw[key]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

// Bool returns a boolean field (false when absent).
func (s Step) Bool(key string) bool {
	var v bool
	if raw, ok := s.raw[key]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

// Raw returns a field's JSON.
func (s Step) Raw(key string) json.RawMessage { return s.raw[key] }

// Spec is a decoded scenarios.json.
type Spec struct {
	// Now is "frozen_now": the instant Python's clock is frozen to.
	Now   time.Time
	Steps []Step
	top   map[string]json.RawMessage
}

// Field returns a top-level field's JSON (for example scenario settings).
func (s Spec) Field(key string) json.RawMessage { return s.top[key] }

// Env is what a step handler receives besides the step and its job.
type Env struct {
	T     *testing.T
	Spec  Spec
	Owner *pgxpool.Pool // schema owner: seeds and inspects, bypasses RLS
	App   *pgxpool.Pool // NOSUPERUSER NOBYPASSRLS runtime role, as the worker uses
}

// Result is the outcome recorded for one step. Job is null for a step that has
// none, like the Python runner's.
type Result struct {
	Job   *int64  `json:"job"`
	Op    string  `json:"op"`
	Error *string `json:"error"`
}

// Replay runs every step through handle. It skips the test unless the harness
// configured it. The job of each step is loaded from the jobs table the way
// the queue would hand it over (including the lease columns, so lease fencing
// behaves as it does in production when a scenario leaves a job claimed).
func Replay(t *testing.T, handle func(ctx context.Context, env *Env, step Step, job queue.Job) error) {
	t.Helper()
	rawURL, out, scenarios := os.Getenv(EnvDatabase), os.Getenv(EnvOut), os.Getenv(EnvScenarios)
	if rawURL == "" || out == "" || scenarios == "" {
		t.Skip("OPENGTM_PARITY_* not set; this test is driven by tests/test_*_go_parity_pg.py")
	}
	ownerURL, err := config.NormalizeDatabaseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := LoadSpec(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	env := &Env{
		T:     t,
		Spec:  spec,
		Owner: dbtest.Pool(t, ownerURL, 2),
		App:   dbtest.Pool(t, jobtest.AppURL(t, ownerURL), 4),
	}

	ctx := context.Background()
	results := make([]Result, 0, len(spec.Steps))
	for _, step := range spec.Steps {
		r := Result{Op: step.Op}
		if step.Job != 0 {
			job := step.Job
			r.Job = &job
		}
		var stepErr error
		if step.Op == "sql" {
			// Generic op: a data change between steps, run as the schema owner.
			_, stepErr = env.Owner.Exec(ctx, step.String("sql"))
		} else {
			var job queue.Job
			if step.Job != 0 { // a step without a job (such as "config") gets a zero Job
				if job, err = LoadJob(ctx, env.Owner, step.Job); err != nil {
					t.Fatalf("job %d: %v", step.Job, err)
				}
			}
			stepErr = handle(ctx, env, step, job)
		}
		if stepErr != nil {
			msg := stepErr.Error()
			r.Error = &msg
		}
		results = append(results, r)
	}
	WriteJSON(t, out, results)
}

// LoadSpec reads a scenarios.json.
func LoadSpec(path string) (Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return Spec{}, err
	}
	var spec Spec
	spec.top = top
	var frozen string
	if f, ok := top["frozen_now"]; ok {
		if err := json.Unmarshal(f, &frozen); err != nil {
			return Spec{}, fmt.Errorf("frozen_now: %w", err)
		}
		if spec.Now, err = time.Parse(time.RFC3339, frozen); err != nil {
			return Spec{}, fmt.Errorf("frozen_now: %w", err)
		}
	}
	var steps []map[string]json.RawMessage
	if s, ok := top["steps"]; ok {
		if err := json.Unmarshal(s, &steps); err != nil {
			return Spec{}, fmt.Errorf("steps: %w", err)
		}
	}
	for _, m := range steps {
		st := Step{raw: m}
		_ = json.Unmarshal(m["op"], &st.Op)
		_ = json.Unmarshal(m["job"], &st.Job)
		_ = json.Unmarshal(m["note"], &st.Note)
		spec.Steps = append(spec.Steps, st)
	}
	return spec, nil
}

// LoadJob reads a job row into the queue.Job the executor is called with.
func LoadJob(ctx context.Context, owner *pgxpool.Pool, id int64) (queue.Job, error) {
	job := queue.Job{ID: id}
	var workerID, ws *string
	var lockedAt *time.Time
	err := owner.QueryRow(ctx, `SELECT type, payload::text, workspace_id, coalesce(retry_count, 0), worker_id, locked_at
		FROM jobs WHERE id = $1`, id).Scan(&job.Type, &job.Payload, &ws, &job.RetryCount, &workerID, &lockedAt)
	if err != nil {
		return queue.Job{}, err
	}
	if ws != nil {
		job.WorkspaceID = *ws
	}
	if workerID != nil && lockedAt != nil {
		job.Lease = queue.Lease{WorkerID: *workerID, LockedAt: *lockedAt}
	}
	return job, nil
}

// WriteJSON writes v as indented JSON for the harness to read.
func WriteJSON(t testing.TB, path string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
