package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerPersonIntelWS: GET /ws/person/enrich, the enrichment WebSocket (Task 20).
func registerPersonIntelWS(r *httpx.Router, _ Deps) {
	const p = "GET /ws/person/enrich"
	r.Handle(p, notImplemented(p))
}
