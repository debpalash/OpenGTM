package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerLeadUtils mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerLeadUtils(r *httpx.Router, _ Deps) {
	r.Handle("POST /api/leads/score", notImplemented("POST /api/leads/score"))
	r.Handle("POST /api/leads/verify-email", notImplemented("POST /api/leads/verify-email"))
	r.Handle("POST /api/leads/tech-stack", notImplemented("POST /api/leads/tech-stack"))
}
