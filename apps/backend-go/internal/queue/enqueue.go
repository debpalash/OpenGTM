package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Enqueue inserts a pending job for the Go worker, mirroring queue_service.add_job (priority 1,
// max_retries 3; executor 'go', the only executor since the plan 15 cutover). q may be
// the pool or a caller's transaction; the insert rolls back with the latter.
// An empty fireKey stores NULL. The payload is JSON-encoded without HTML
// escaping and keeps struct/slice key order.
func Enqueue(ctx context.Context, q gen.DBTX, jobType, workspaceID string, payload any, fireKey string) (int64, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return 0, fmt.Errorf("queue: encode %s payload: %w", jobType, err)
	}
	id, err := gen.New(q).EnqueueDefaultJob(ctx, gen.EnqueueDefaultJobParams{
		JobType:     jobType,
		Payload:     bytes.TrimRight(buf.Bytes(), "\n"),
		WorkspaceID: workspaceID,
		FireKey:     fireKey,
	})
	if err != nil {
		return 0, fmt.Errorf("queue: enqueue %s: %w", jobType, err)
	}
	return int64(id), nil
}
