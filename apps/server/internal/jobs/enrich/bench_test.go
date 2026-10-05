package enrich

// Enrichment benchmark, Go side (see benchmarks/README.md, "Enrichment").
//
// A test only so it can drive the unexported-free Worker.Handle against a
// database the orchestrator (benchmarks/run_enrich.py) has already seeded with
// one workbook and one processing job, without registering anything in
// production. It is skipped unless OPENGTM_BENCH_OUT is set. The provider is
// the shared HTTPS simulator; trust comes from SSL_CERT_FILE, which the Go
// standard library honours, so the egress client is the production one except
// for the loopback allowance a local simulator needs.

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// peakRSSMB is VmHWM, the process's peak resident set size.
func peakRSSMB() float64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			kib, _ := strconv.ParseFloat(strings.Fields(rest)[0], 64)
			return kib / 1024
		}
	}
	return 0
}

func TestBenchEnrich(t *testing.T) {
	out := os.Getenv("OPENGTM_BENCH_OUT")
	if out == "" {
		t.Skip("OPENGTM_BENCH_OUT not set; run benchmarks/run_enrich.py")
	}
	rawURL := os.Getenv("OPENGTM_BENCH_DATABASE_URL")
	jobID, err := strconv.ParseInt(os.Getenv("OPENGTM_BENCH_JOB"), 10, 64)
	if rawURL == "" || err != nil {
		t.Fatal("OPENGTM_BENCH_DATABASE_URL and OPENGTM_BENCH_JOB are required")
	}
	appURL, err := config.NormalizeDatabaseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	app := dbtest.Pool(t, appURL, 32)
	client, err := egress.New(egress.Options{Version: "bench", DefaultRPS: 1e6, AllowPrivateForTesting: true})
	if err != nil {
		t.Fatal(err)
	}
	w := NewWorker(app, loadConnectors(t, os.Getenv("OPENGTM_BENCH_CONNECTORS")), client, nil, 1e6)

	ctx := context.Background()
	job := queue.Job{ID: jobID, Type: JobType}
	var workerID *string
	var ws *string
	if err := app.QueryRow(ctx, `SELECT payload::text, workspace_id, coalesce(retry_count, 0), worker_id, locked_at
		FROM jobs WHERE id = $1`, jobID).Scan(&job.Payload, &ws, &job.RetryCount, &workerID, &job.Lease.LockedAt); err != nil {
		t.Fatal(err)
	}
	if ws != nil {
		job.WorkspaceID = *ws
	}
	if workerID != nil {
		job.Lease.WorkerID = *workerID
	}
	start := time.Now()
	if err := w.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	wall := time.Since(start).Seconds()
	b, _ := json.Marshal(map[string]any{"engine": "go", "wall_s": wall, "max_rss_mb": peakRSSMB()})
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
