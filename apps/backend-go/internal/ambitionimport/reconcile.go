package ambitionimport

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// Reconcile is reconcile_ambitionbox_job_failure: after the queue persisted a
// failed attempt, make the run and workbook agree (retrying vs failed). It is
// a no-op for finished runs, for jobs without a workspace or whose payload names another one, and for
// malformed payloads.
func Reconcile(ctx context.Context, d Deps, n queue.FailureNotice) error {
	p, ok := payloadIDs(n.Payload)
	if !ok {
		d.log().Error("ambitionbox failure hook: invalid payload", "job_id", n.JobID)
		return nil
	}
	if n.WorkspaceID == "" || n.WorkspaceID != p.ws { // the tenant is the job row's only
		d.log().Error("ambitionbox failure hook: payload workspace differs from the job's", "job_id", n.JobID)
		return nil
	}
	status := "failed"
	if n.WillRetry {
		status = "retrying"
	}
	return settle(ctx, d, p, status, n.Reason)
}

// RegisterFailureHook wires Reconcile as the failure hook of ambitionbox_import.
func RegisterFailureHook(d Deps) {
	queue.RegisterFailureHook(JobType, func(ctx context.Context, n queue.FailureNotice) error {
		return Reconcile(ctx, d, n)
	})
}
