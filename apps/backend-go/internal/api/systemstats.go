package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerSystemStats mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerSystemStats(r *httpx.Router, _ Deps) {
	r.Handle("GET /api/system-stats", notImplemented("GET /api/system-stats"))
}
