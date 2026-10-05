package enrich

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// TestParityDriver is the Go half of the cross-language harness
// (tests/test_enrichment_go_parity_pg.py). It is inert unless the harness sets
// the OPENGTM_PARITY_* variables. It runs the job rows the harness inserted
// through the real handler, as a NOSUPERUSER NOBYPASSRLS role, and records
// what each step returned; the harness compares the resulting database state
// with the Python runner's.
//
//	TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
//	  uv run pytest tests/test_enrichment_go_parity_pg.py
func TestParityDriver(t *testing.T) {
	rawURL, out := os.Getenv("OPENGTM_PARITY_DATABASE_URL"), os.Getenv("OPENGTM_PARITY_OUT")
	stepsPath, connectors := os.Getenv("OPENGTM_PARITY_STEPS"), os.Getenv("OPENGTM_PARITY_CONNECTORS")
	if rawURL == "" || out == "" || stepsPath == "" || connectors == "" {
		t.Skip("OPENGTM_PARITY_* not set; this test is driven by tests/test_enrichment_go_parity_pg.py")
	}
	ownerURL, err := config.NormalizeDatabaseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(stepsPath)
	if err != nil {
		t.Fatal(err)
	}
	var steps []struct {
		Job int64 `json:"job"`
	}
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatal(err)
	}

	owner := dbtest.Pool(t, ownerURL, 2)
	app := dbtest.Pool(t, dbtest.AppURL(t, ownerURL), 16)
	client, err := egress.New(egress.Options{Version: "parity", DefaultRPS: 1000, AllowPrivateForTesting: true})
	if err != nil {
		t.Fatal(err)
	}
	w := NewWorker(app, loadConnectors(t, connectors), client, nil, 1000)

	ctx := context.Background()
	type result struct {
		Job   int64   `json:"job"`
		Error *string `json:"error"`
	}
	results := make([]result, 0, len(steps))
	for _, s := range steps {
		job := queue.Job{ID: s.Job, Type: JobType}
		var workerID *string
		var ws *string
		if err := owner.QueryRow(ctx, `SELECT payload::text, workspace_id, coalesce(retry_count, 0), worker_id, locked_at
			FROM jobs WHERE id = $1`, s.Job).Scan(&job.Payload, &ws, &job.RetryCount, &workerID, &job.Lease.LockedAt); err != nil {
			t.Fatalf("job %d: %v", s.Job, err)
		}
		if ws != nil {
			job.WorkspaceID = *ws
		}
		if workerID != nil {
			job.Lease.WorkerID = *workerID
		}
		r := result{Job: s.Job}
		if err := w.Handle(ctx, job); err != nil {
			msg := err.Error()
			r.Error = &msg
		}
		results = append(results, r)
	}
	b, err := json.MarshalIndent(results, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
