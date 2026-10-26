package destsync

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// failurePayload is reconcile_destination_job_failure's guard: both ids are stripped and
// must be non-blank; the job row's workspace must agree.
func failurePayload(raw []byte, claimWS string) (ws, run string, err error) {
	ws, run, ok := payloadIDs(raw)
	ws, run = strings.TrimSpace(ws), strings.TrimSpace(run)
	if !ok || ws == "" || run == "" {
		return "", "", errors.New(errFailurePayload)
	}
	if claimWS != "" && claimWS != ws {
		return "", "", ErrWorkspaceMismatch
	}
	return ws, run, nil
}

// OnFailure is reconcile_destination_job_failure: a killed or failed queue attempt is
// mirrored into the run (deststore.ReconcileFailure). The workspace named by the payload
// must be the job row's own; the run is then read inside that tenant, so another tenant's
// run id finds nothing and is left alone.
func (e *Engine) OnFailure(ctx context.Context, pool *pgxpool.Pool, c *queue.Claim, errText string, willRetry bool) error {
	if c == nil {
		return errors.New(errFailurePayload)
	}
	ws, runID, err := failurePayload(c.Payload, c.WorkspaceID)
	if err != nil {
		return err
	}
	return tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		run, err := deststore.GetRun(ctx, tx, runID)
		if errors.Is(err, deststore.ErrRunNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if run.WorkspaceID != ws {
			return ErrWorkspaceMismatch
		}
		return deststore.ReconcileFailure(ctx, tx, run, willRetry, cleanError(errText))
	})
}

// Eligible reports whether the Go worker may run a destination type: it must be listed in
// GO_DEST_TYPES and deliverable with the wired ports. The worker hands every other row back
// to Python (queue.HandBack).
func Eligible(destType string, ports destdeliver.Ports, goTypes map[string]bool) bool {
	return goTypes[destType] && ports.Supports(destType)
}
