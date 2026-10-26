package api

import (
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// copilotRoutesEnabled: COPILOT_GO_ENABLED and PG_LEAD_STORE on. Otherwise nothing registers and
// the reason is logged, so an nginx canary cannot be enabled by mistake.
func (d Deps) copilotRoutesEnabled(area string) bool {
	if !d.Flags.Copilot.GoCopilotEnabled {
		d.logger().Warn("copilot routes not registered: COPILOT_GO_ENABLED is false", "area", area)
		return false
	}
	if !d.Flags.PGLeadStore {
		d.logger().Warn("copilot routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
		return false
	}
	return true
}

// notImplemented is the placeholder handler of a scaffolded route until its task lands
// (Tasks 17-20). Unreachable in production: the group is gated by COPILOT_GO_ENABLED.
func notImplemented(pattern string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.Detail(w, http.StatusServiceUnavailable, "not implemented: "+pattern)
	})
}

// RegisterCopilot registers the 7 /api/copilotkit routes (Tasks 17-18). Chat itself
// (POST /api/copilotkit) needs the Python worker (PyWorker non-nil); the rest do not.
func RegisterCopilot(r *httpx.Router, d Deps) {
	if !d.copilotRoutesEnabled("copilot") {
		return
	}
	registerCopilotRoutes(r, d)
	if d.PyWorker == nil {
		d.logger().Warn("copilot chat route not registered: PyWorker is not configured", "area", "copilot")
		return
	}
	registerCopilotChat(r, d)
}

// RegisterPersonIntel registers the 5 person intel routes (Tasks 19-20).
func RegisterPersonIntel(r *httpx.Router, d Deps) {
	if !d.copilotRoutesEnabled("person-intel") {
		return
	}
	registerPersonIntelShared(r, d)
}
