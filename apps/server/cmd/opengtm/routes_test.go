package main

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	_ "github.com/debpalash/OpenGTM/apps/server/internal/jobs/audiencerefresh"
	_ "github.com/debpalash/OpenGTM/apps/server/internal/jobs/playbooksched"
	_ "github.com/debpalash/OpenGTM/apps/server/internal/jobs/retention"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

func TestRoutesSetRefusesATypeThisBinaryCannotRun(t *testing.T) {
	owner := dbtest.OwnerURL(t)
	dbtest.Migrate(t, owner)
	pool := dbtest.Pool(t, owner, 2)
	t.Setenv("DATABASE_URL", dbtest.AppURL(t, owner))
	t.Setenv("OPENGTM_CONFIG", "")
	ctx := context.Background()
	const phantom = "routes_test_phantom_type"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM job_executor_routes WHERE job_type = $1`, phantom) })

	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_executor_routes WHERE job_type = $1`, phantom).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := runRoutes(ctx, []string{"set", phantom, "go"}); err == nil || err.Error() != "route unchanged" {
		t.Fatalf("routing a type with no Go executor must be refused, got %v", err)
	}
	if count() != 0 {
		t.Fatal("the refused route was written")
	}
	if err := runRoutes(ctx, []string{"set", phantom, "python"}); err != nil || count() != 1 {
		t.Fatalf("routing to python is always allowed: %v", err)
	}
	if err := runRoutes(ctx, []string{"set", phantom, "go", "--force"}); err != nil {
		t.Fatalf("--force must override: %v", err)
	}
}

func TestEveryPortedTypeIsASwitchableExecutor(t *testing.T) {
	for _, jobType := range []string{"retention_enforce", "research_playbook_schedule", "audience_refresh"} {
		if !queue.HasExecutor(jobType) {
			t.Errorf("%s has a Go executor in this binary but did not declare it", jobType)
		}
	}
	// plugin_run is Go-only, not a switchable Python default.
	if !queue.HasExecutor("plugin_run") {
		t.Error("plugin_run must be declared as an executor")
	}
	for _, jobType := range queue.Switchable() {
		if jobType == "plugin_run" {
			t.Error("plugin_run must not be listed as a Python-default switchable type")
		}
	}
}
