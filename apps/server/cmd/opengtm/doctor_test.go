package main

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

func TestParseInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	force := fs.Bool("force", false, "")
	cfg := fs.String("config", "", "")
	pos, err := parseInterleaved(fs, []string{"plugin_run", "--force", "go", "--config", "x.yaml"})
	if err != nil || !slices.Equal(pos, []string{"plugin_run", "go"}) || !*force || *cfg != "x.yaml" {
		t.Fatalf("pos=%v force=%v cfg=%q err=%v", pos, *force, *cfg, err)
	}
	if _, err := parseInterleaved(fs, []string{"--nope"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestPrintRoutes(t *testing.T) {
	var buf bytes.Buffer
	printRoutes(&buf, []queue.Route{{JobType: "plugin_run", Executor: "go", Pending: 2}}, false)
	if !strings.Contains(buf.String(), "plugin_run") || !strings.Contains(buf.String(), "claimed by Python") {
		t.Fatalf("table output:\n%s", buf.String())
	}
	buf.Reset()
	printRoutes(&buf, []queue.Route{{JobType: "retention_enforce", Executor: "python", Default: true, GoExecutor: true}}, false)
	if !strings.Contains(buf.String(), "retention_enforce") || !strings.Contains(buf.String(), "python (default, switchable)") {
		t.Fatalf("switchable default not shown:\n%s", buf.String())
	}
	buf.Reset()
	printRoutes(&buf, []queue.Route{{JobType: "mystery", Executor: "go"}, {JobType: "plugin_run", Executor: "go", GoExecutor: true}}, false)
	if !strings.Contains(buf.String(), "go (NO EXECUTOR IN THIS BINARY: jobs are not claimed)") ||
		strings.Count(buf.String(), "NO EXECUTOR") != 1 {
		t.Fatalf("a route nothing claims must be flagged:\n%s", buf.String())
	}
	buf.Reset()
	printRoutes(&buf, nil, true)
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Fatalf("empty JSON = %q", buf.String())
	}
}

func TestDoctorFlagsPrivilegedRoles(t *testing.T) {
	owner := dbtest.OwnerURL(t)
	dbtest.Migrate(t, owner)
	app := dbtest.AppURL(t, owner)
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
		}
	}))
	defer legacy.Close()
	t.Setenv("OPENGTM_LEGACY_API_URL", legacy.URL)
	t.Setenv("OPENGTM_CONFIG", "")

	status := func(url string) map[string]bool {
		t.Setenv("DATABASE_URL", url)
		got := map[string]bool{}
		for _, r := range doctorChecks(context.Background(), "") {
			got[r.Name] = r.OK
		}
		return got
	}
	got := status(app)
	for _, name := range []string{"config", "database", "runtime role", "migrations", "executor routes", "go executors", "legacy api"} {
		if !got[name] {
			t.Errorf("app role: check %q failed: %v", name, got)
		}
	}
	if got := status(owner); got["runtime role"] {
		t.Error("superuser connection must fail the runtime role check")
	}
}

func TestDoctorFlagsARouteNothingClaims(t *testing.T) {
	owner := dbtest.OwnerURL(t)
	dbtest.Migrate(t, owner)
	app := dbtest.AppURL(t, owner)
	pool := dbtest.Pool(t, app, 2)
	ownerPool := dbtest.Pool(t, owner, 2)
	ctx := context.Background()
	const stranded = "doctor_stranded_type"
	t.Cleanup(func() {
		_, _ = ownerPool.Exec(ctx, `DELETE FROM job_executor_routes WHERE job_type = $1`, stranded)
	})

	if got := executorCheck(ctx, pool); !got.OK {
		t.Fatalf("a clean install must pass: %+v", got)
	}
	if _, err := ownerPool.Exec(ctx, `INSERT INTO job_executor_routes (job_type, executor) VALUES ($1, 'go')
		ON CONFLICT (job_type) DO UPDATE SET executor = 'go'`, stranded); err != nil {
		t.Fatal(err)
	}
	got := executorCheck(ctx, pool)
	if got.OK || !strings.Contains(got.Detail, stranded) || !strings.Contains(got.Detail, "routes set <type> python") {
		t.Fatalf("a route to go without an executor must fail the check: %+v", got)
	}
	// A route to a type this binary can run (plugin_run is seeded to go) is fine.
	if _, err := ownerPool.Exec(ctx, `UPDATE job_executor_routes SET executor = 'python' WHERE job_type = $1`, stranded); err != nil {
		t.Fatal(err)
	}
	if got := executorCheck(ctx, pool); !got.OK || !strings.Contains(got.Detail, "plugin_run") {
		t.Fatalf("plugin_run runs in go and must be listed: %+v", got)
	}
}
