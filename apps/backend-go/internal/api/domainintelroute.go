package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerDomainIntel mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerDomainIntel(r *httpx.Router, _ Deps) {
	r.Handle("POST /api/leads/domain-intel", notImplemented("POST /api/leads/domain-intel"))
}
