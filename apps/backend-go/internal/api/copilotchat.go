package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerCopilotChat: POST /api/copilotkit, the SSE chat turn including approved_tool_calls (Task 17).
func registerCopilotChat(r *httpx.Router, _ Deps) {
	const p = "POST /api/copilotkit"
	r.Handle(p, notImplemented(p))
}
