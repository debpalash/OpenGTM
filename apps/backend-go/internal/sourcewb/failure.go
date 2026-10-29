package sourcewb

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// RegisterFailureHook makes a source_workbook job that spent its retries (or died: the reaper reports it the same
// way) leave its workbook "draft" instead of "running" forever. Python had no such hook (final correctness minor 2).
func RegisterFailureHook(pool *pgxpool.Pool) {
	queue.RegisterFailureHook(JobType, failureHook(pool))
}

// failureHook: the tenant is the failed row's workspace_id only (a row without one is left alone). Only a final
// failure resets the workbook, and only while it is still "running": a cancelled workbook stays cancelled and a
// workbook another job already moved on is not touched.
func failureHook(pool *pgxpool.Pool) queue.FailureHook {
	return func(ctx context.Context, n queue.FailureNotice) error {
		if n.WillRetry || n.WorkspaceID == "" {
			return nil
		}
		var p payload
		if err := json.Unmarshal(n.Payload, &p); err != nil || p.WorkbookID == "" {
			return nil
		}
		return tenancy.InWorkspace(ctx, pool, n.WorkspaceID, func(tx pgx.Tx) error {
			return gen.New(tx).SWResetRunning(ctx, gen.SWResetRunningParams{ID: p.WorkbookID, WorkspaceID: n.WorkspaceID})
		})
	}
}
