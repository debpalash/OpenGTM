package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// ErrNoWorkspace: collect and bulk_enrich rows always carry their tenant (the Go handlers take it
// only from jobs.workspace_id).
var ErrNoWorkspace = errors.New("collect: enqueue without a workspace")

// Enqueue is queue_service.add_job(priority 1, max_retries 3) for collect / bulk_enrich. executor must
// be "go" (config.CollectExecutor / BulkEnrichExecutor; the only executor since the cutover). q may be the caller's
// transaction. The payload keeps struct key order, without HTML escaping.
func Enqueue(ctx context.Context, q gen.DBTX, jobType, workspaceID, executor string, payload any, fireKey string) (int64, error) {
	if workspaceID == "" {
		return 0, ErrNoWorkspace
	}
	if executor != "go" {
		return 0, fmt.Errorf("collect: executor %q must be go (the only executor since the plan 15 cutover)", executor)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return 0, fmt.Errorf("collect: encode %s payload: %w", jobType, err)
	}
	id, err := gen.New(q).EnqueueJobFor(ctx, gen.EnqueueJobForParams{Executor: executor, JobType: jobType,
		Payload: bytes.TrimRight(buf.Bytes(), "\n"), WorkspaceID: workspaceID, FireKey: fireKey})
	if err != nil {
		return 0, fmt.Errorf("collect: enqueue %s: %w", jobType, err)
	}
	return int64(id), nil
}
