package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerCopilotRoutes: info (GET/POST), conversations (list/get/delete), memories (Task 18).
func registerCopilotRoutes(r *httpx.Router, _ Deps) {
	for _, p := range []string{
		"GET /api/copilotkit/info",
		"POST /api/copilotkit/info",
		"GET /api/copilotkit/conversations",
		"GET /api/copilotkit/conversations/{conv_id}",
		"DELETE /api/copilotkit/conversations/{conv_id}",
		"GET /api/copilotkit/memories",
	} {
		r.Handle(p, notImplemented(p))
	}
}
