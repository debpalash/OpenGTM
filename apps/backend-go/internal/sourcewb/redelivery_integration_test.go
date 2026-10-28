//go:build integration

package sourcewb

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func setWBStatus(t *testing.T, owner *pgxpool.Pool, wb, st string) {
	t.Helper()
	if _, err := owner.Exec(context.Background(), "UPDATE workbooks SET status = $2 WHERE id = $1", wb, st); err != nil {
		t.Fatal(err)
	}
}

// Final correctness minor 2: a source_workbook job that spent its retries must not leave the workbook "running".
func TestFailureHookResetsRunningWorkbookOnFinalFailure(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	owner := ownerPool(t)
	ws, wb := seedCollectWorkbook(t, owner)
	hook := failureHook(pool)
	body, _ := json.Marshal(JobPayload{WorkbookID: wb, ColumnID: "src_1", WorkspaceID: ws})
	n := queue.FailureNotice{JobID: 1, JobType: JobType, WorkspaceID: ws, Payload: body, Reason: "boom"}
	ctx := context.Background()

	setWBStatus(t, owner, wb, "running")
	n.WillRetry = true
	if err := hook(ctx, n); err != nil || wbStatus(t, owner, wb) != "running" {
		t.Fatalf("a retry is coming: status %q err %v", wbStatus(t, owner, wb), err)
	}
	n.WillRetry = false
	if err := hook(ctx, n); err != nil || wbStatus(t, owner, wb) != "draft" {
		t.Fatalf("final failure: status %q err %v", wbStatus(t, owner, wb), err)
	}
	setWBStatus(t, owner, wb, StatusCancelled)
	if err := hook(ctx, n); err != nil || wbStatus(t, owner, wb) != StatusCancelled {
		t.Fatalf("a cancelled workbook must stay cancelled: %q err %v", wbStatus(t, owner, wb), err)
	}
	n.WorkspaceID = ""
	setWBStatus(t, owner, wb, "running")
	if err := hook(ctx, n); err != nil || wbStatus(t, owner, wb) != "running" {
		t.Fatalf("a job without a workspace is left alone: %q err %v", wbStatus(t, owner, wb), err)
	}
}

// Final correctness minor 3: enrich_after chaining survives a redelivery. Attempt 1 commits the rows and dies, so
// attempt 2 adds nothing but finds them (skipped); the run_workbook follow-up is enqueued exactly once per source job.
func TestEnrichAfterChainIsOncePerSourceJobAndSurvivesRedelivery(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	owner := ownerPool(t)
	ws, wb := seedCollectWorkbook(t, owner)
	d := Deps{Pool: pool}
	ctx := context.Background()
	count := func() (n int) {
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type = 'run_workbook' AND workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := owner.Exec(ctx, `DELETE FROM jobs WHERE type = 'run_workbook' AND workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if err := d.chainRun(ctx, ws, wb, 4242); err != nil || count() != 1 {
		t.Fatalf("first chain: n=%d err=%v", count(), err)
	}
	// The follow-up already ran to completion; the redelivered attempt must not enqueue it again.
	if _, err := owner.Exec(ctx, `UPDATE jobs SET status = 'completed' WHERE type = 'run_workbook' AND workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if err := d.chainRun(ctx, ws, wb, 4242); err != nil || count() != 1 {
		t.Fatalf("redelivered chain: n=%d err=%v", count(), err)
	}
	if err := d.chainRun(ctx, ws, wb, 4243); err != nil || count() != 2 {
		t.Fatalf("another source job chains its own run: n=%d err=%v", count(), err)
	}
}
