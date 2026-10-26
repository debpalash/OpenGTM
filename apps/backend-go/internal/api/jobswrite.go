package api

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Test seams. nil means the production behaviour.
var (
	jobsCancelQueue  func(ctx context.Context, tx pgx.Tx, ws, id string, now time.Time) (int, error)
	jobsRetryEnqueue func(ctx context.Context, tx pgx.Tx, ws string, payload any, fireKey string) (id int64, created bool, err error)
)

func parseKeepLeads(values []string) (bool, *httpx.FieldError) { return false, nil }

// registerJobsWrite mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerJobsWrite(r *httpx.Router, _ Deps) {
	r.Handle("POST /api/jobs/{job_id}/cancel", notImplemented("POST /api/jobs/{job_id}/cancel"))
	r.Handle("DELETE /api/jobs/{job_id}", notImplemented("DELETE /api/jobs/{job_id}"))
	r.Handle("POST /api/jobs/{job_id}/retry", notImplemented("POST /api/jobs/{job_id}/retry"))
}
