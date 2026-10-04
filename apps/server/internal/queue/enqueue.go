package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is satisfied by pgx.Tx, *pgx.Conn and *pgxpool.Pool.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// EnqueueOptions mirror QueueService.add_job's keyword arguments.
type EnqueueOptions struct {
	// Priority defaults to 1 when zero; higher runs first.
	Priority int
	// MaxRetries defaults to 3 when nil.
	MaxRetries *int
	// FireKey is a single-flight key; a second active job with the same key
	// fails with ErrDuplicateFireKey and aborts the caller's transaction.
	FireKey string
	// RunAt delays eligibility; zero means now.
	RunAt time.Time
}

// ErrDuplicateFireKey reports an active job already holding the fire_key.
var ErrDuplicateFireKey = errors.New("queue: an active job already holds this fire_key")

const enqueueSQL = `
INSERT INTO jobs (type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES ($1, $2::json, $3, $4, $5, 'pending', LOCALTIMESTAMP,
        COALESCE($6::timestamptz::timestamp, LOCALTIMESTAMP), $7, 0)
RETURNING id`

// Enqueue inserts a pending job inside the caller's transaction, so the job
// becomes visible only if the domain records created alongside it commit.
// workspace_id is derived from payload["workspace_id"] as add_job does.
func Enqueue(ctx context.Context, tx Querier, jobType string, payload any, opts EnqueueOptions) (int64, error) {
	if jobType == "" {
		return 0, errors.New("queue: enqueue requires a job type")
	}
	raw, err := encodePayload(payload)
	if err != nil {
		return 0, err
	}
	var probe struct {
		WorkspaceID any `json:"workspace_id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return 0, fmt.Errorf("queue: payload must be a JSON object: %w", err)
	}
	workspace := pythonStr(probe.WorkspaceID)

	priority := opts.Priority
	if priority == 0 {
		priority = 1
	}
	maxRetries := 3
	if opts.MaxRetries != nil {
		maxRetries = *opts.MaxRetries
	}
	var fireKey *string
	if opts.FireKey != "" {
		fireKey = &opts.FireKey
	}
	var runAt *time.Time
	if !opts.RunAt.IsZero() {
		runAt = &opts.RunAt
	}
	var id int64
	err = tx.QueryRow(ctx, enqueueSQL, jobType, raw, workspace, priority, fireKey, runAt, maxRetries).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_jobs_fire_key_active" {
			return 0, ErrDuplicateFireKey
		}
		return 0, fmt.Errorf("queue: enqueue %s: %w", jobType, err)
	}
	return id, nil
}

func encodePayload(payload any) (json.RawMessage, error) {
	var raw []byte
	switch p := payload.(type) {
	case nil:
		raw = []byte("{}")
	case json.RawMessage:
		raw = p
	case []byte:
		raw = p
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("queue: encode payload: %w", err)
		}
		raw = b
	}
	if !json.Valid(raw) {
		return nil, errors.New("queue: payload is not valid JSON")
	}
	return raw, nil
}

// pythonStr reproduces `str(v) if v else None` for JSON-decoded values, so a
// Go-enqueued job carries the same workspace_id Python would derive.
func pythonStr(v any) *string {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case float64:
		if t == 0 {
			return nil
		}
		s = strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if !t {
			return nil
		}
		s = "True"
	default:
		return nil
	}
	if s == "" {
		return nil
	}
	return &s
}
