package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerJobsWrite mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerJobsWrite(r *httpx.Router, _ Deps) {
	r.Handle("POST /api/jobs/{job_id}/cancel", notImplemented("POST /api/jobs/{job_id}/cancel"))
	r.Handle("DELETE /api/jobs/{job_id}", notImplemented("DELETE /api/jobs/{job_id}"))
	r.Handle("POST /api/jobs/{job_id}/retry", notImplemented("POST /api/jobs/{job_id}/retry"))
}
