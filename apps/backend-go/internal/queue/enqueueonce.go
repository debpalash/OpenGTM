package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// EnqueueOnce inserts one active job for fireKey, mirroring
// job_scheduling.enqueue_job_once: a transaction-scoped advisory lock on the
// key hash, no second pending/processing row for the same key, and the partial
// unique index uq_jobs_fire_key_active as backstop. created is false (id 0)
// when an active occurrence already exists. executor is explicit (only 'go' is accepted since
// the plan 15 cutover). The payload must encode to a JSON object; as in Python the stored
// payload is {**payload, "fire_key": fireKey} (fire_key last). The insert
// rolls back with tx.
func EnqueueOnce(ctx context.Context, tx pgx.Tx, jobType, workspaceID string, payload any, fireKey, executor string) (id int64, created bool, err error) {
	if fireKey == "" {
		return 0, false, errors.New("queue: EnqueueOnce needs a fire key")
	}
	if executor != "go" {
		return 0, false, fmt.Errorf("queue: EnqueueOnce executor %q must be go (the only executor since the plan 15 cutover)", executor)
	}
	body, err := onceBody(jobType, payload, fireKey)
	if err != nil {
		return 0, false, err
	}
	q := gen.New(tx)
	if err := q.LockJobFireKey(ctx, fireKey); err != nil {
		return 0, false, fmt.Errorf("queue: lock %s: %w", jobType, err)
	}
	if _, err := q.ActiveJobByFireKey(ctx, fireKey); err == nil {
		return 0, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("queue: look up %s: %w", jobType, err)
	}
	newID, err := q.InsertJobOnce(ctx, gen.InsertJobOnceParams{
		Executor: executor, JobType: jobType, Payload: body, WorkspaceID: workspaceID, FireKey: fireKey,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil // lost the unique-index race
	}
	if err != nil {
		return 0, false, fmt.Errorf("queue: enqueue once %s: %w", jobType, err)
	}
	return int64(newID), true, nil
}

func onceBody(jobType string, payload any, fireKey string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("queue: encode %s payload: %w", jobType, err)
	}
	b := bytes.TrimSpace(buf.Bytes())
	if len(b) < 2 || b[0] != '{' || b[len(b)-1] != '}' {
		return nil, fmt.Errorf("queue: %s payload must be a JSON object", jobType)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("queue: %s payload: %w", jobType, err)
	}
	if _, dup := keys["fire_key"]; dup {
		return nil, fmt.Errorf("queue: %s payload already has a fire_key", jobType)
	}
	var kb bytes.Buffer
	kenc := json.NewEncoder(&kb)
	kenc.SetEscapeHTML(false)
	_ = kenc.Encode(fireKey)
	out := append([]byte(nil), b[:len(b)-1]...)
	if len(keys) > 0 {
		out = append(out, ',')
	}
	out = append(out, `"fire_key":`...)
	out = append(out, bytes.TrimSpace(kb.Bytes())...)
	return append(out, '}'), nil
}
