package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerPersonIntelRoutes: enrich, history, get and delete by record_id (Task 19).
// The literal /api/person/history is registered beside /api/person/{record_id}; ServeMux
// precedence makes the literal win like FastAPI's declaration order.
func registerPersonIntelRoutes(r *httpx.Router, _ Deps) {
	for _, p := range []string{
		"POST /api/person/enrich",
		"GET /api/person/history",
		"GET /api/person/{record_id}",
		"DELETE /api/person/{record_id}",
	} {
		r.Handle(p, notImplemented(p))
	}
}
