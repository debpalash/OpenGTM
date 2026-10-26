package audiencesched

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func ReconcileFailure(ctx context.Context, pool *pgxpool.Pool, executorFor func(ws string) string, jobID int64, payload map[string]any, errText string, willRetry bool, now time.Time) error {
	return errNotImplemented
}

func DueSchedules(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]DueRow, error) {
	return nil, errNotImplemented
}
