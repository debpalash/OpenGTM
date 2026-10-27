//go:build integration

package refresh_test

import (
	"errors"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/refresh"
)

// Review 3b L6: the tenant is the job's workspace_id column only. A claim without one is refused, even when the
// payload names a workspace (a payload is writable by whoever enqueued the row).
func TestRefreshRefusesAnEmptyClaimWorkspace(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	err := refresh.Handle(ctx, deps(pool), claim(1001, "", map[string]any{"workbook_id": "w1", "workspace_id": wsA}))
	var perm *queue.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want a PermanentError", err)
	}
	if n := len(jobs(t, ctx, pool, wsA)); n != 0 {
		t.Errorf("an unscoped claim enqueued %d jobs", n)
	}
	if n, _ := activityCount(t, ctx, pool, wsA); n != 0 {
		t.Errorf("an unscoped claim wrote %d activity rows", n)
	}
}
