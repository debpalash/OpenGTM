package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerCollectRoute mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerCollectRoute(r *httpx.Router, _ Deps) {
	r.Handle("POST /api/collect", notImplemented("POST /api/collect"))
}
