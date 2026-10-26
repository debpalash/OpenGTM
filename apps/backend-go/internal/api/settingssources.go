package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerSourceSettings mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerSourceSettings(r *httpx.Router, _ Deps) {
	r.Handle("GET /api/settings/sources", notImplemented("GET /api/settings/sources"))
	r.Handle("GET /api/settings/sources/health", notImplemented("GET /api/settings/sources/health"))
	r.Handle("GET /api/settings/sources/registry", notImplemented("GET /api/settings/sources/registry"))
	r.Handle("PUT /api/settings/sources/{source_id}", notImplemented("PUT /api/settings/sources/{source_id}"))
	r.Handle("POST /api/settings/sources/{source_id}/health/reset", notImplemented("POST /api/settings/sources/{source_id}/health/reset"))
}
