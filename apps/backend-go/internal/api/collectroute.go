package api

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectintent"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// Test seams of POST /api/collect (declared here so the RED tests compile).
var (
	collectNewID   func() string
	collectEnqueue func(ctx context.Context, tx pgx.Tx, ws string, payload any, fireKey string) (int64, error)
	collectBus     func(d Deps) *leadprogress.Bus
)

// collectEnqueueDefault enqueues the collect job on the Python queue (executor python, as add_job).
func collectEnqueueDefault(ctx context.Context, tx pgx.Tx, ws string, payload any, fireKey string) (int64, error) {
	return queue.Enqueue(ctx, tx, "collect", ws, payload, fireKey)
}

// collectIntentDetails is json.dumps(decision.to_dict()) (default separators, ensure_ascii).
func collectIntentDetails(_ collectintent.Decision) string { return "" }

// registerCollectRoute mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerCollectRoute(r *httpx.Router, _ Deps) {
	r.Handle("POST /api/collect", notImplemented("POST /api/collect"))
}
