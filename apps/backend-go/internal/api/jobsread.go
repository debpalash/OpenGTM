package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// registerJobsRead mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerJobsRead(r *httpx.Router, _ Deps) {
	r.Handle("GET /api/jobs", notImplemented("GET /api/jobs"))
	r.Handle("GET /api/jobs/{job_id}", notImplemented("GET /api/jobs/{job_id}"))
	r.Handle("GET /api/jobs/{job_id}/stages", notImplemented("GET /api/jobs/{job_id}/stages"))
	r.Handle("GET /api/jobs/{job_id}/leads", notImplemented("GET /api/jobs/{job_id}/leads"))
	r.Handle("GET /api/jobs/{job_id}/events", notImplemented("GET /api/jobs/{job_id}/events"))
}
