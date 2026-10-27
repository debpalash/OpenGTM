//go:build integration

package refresh_test

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/refresh"
)

// TestRefreshSourceExecutorFollowsCohort: with the GO_EXECUTOR_SOURCE_WORKBOOK cohort the refresh
// chain enqueues its source_workbook jobs for the Go worker (stamped with the workspace).
func TestRefreshSourceExecutorFollowsCohort(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	d := deps(pool)
	d.SourceExecutorFor = func(ws string) string {
		if ws == wsA {
			return "go"
		}
		return "python"
	}
	if err := refresh.Handle(ctx, d, claim(511, wsA, map[string]any{"workbook_id": "w1", "workspace_id": wsA})); err != nil {
		t.Fatal(err)
	}
	src := ofType(jobs(t, ctx, pool, wsA), "source_workbook")
	if len(src) != 1 || src[0].Executor != "go" || src[0].Payload["workspace_id"] != wsA {
		t.Fatalf("source job: %+v", src)
	}
}
