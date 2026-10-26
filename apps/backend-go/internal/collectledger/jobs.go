package collectledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Job is one collection_jobs row. Field order is the JSON key order Python emits
// (the ledger's explicit column list: workspace_id last). Timestamps are the stored
// strings; intent_details is the raw stored text.
type Job struct {
	ID            string `json:"id"`
	Query         string `json:"query"`
	Intent        string `json:"intent"`
	IntentDetails string `json:"intent_details"`
	Status        string `json:"status"`
	Tier          int32  `json:"tier"`
	Attempts      int32  `json:"attempts"`
	MaxAttempts   int32  `json:"max_attempts"`
	LeadsFound    int32  `json:"leads_found"`
	ProxyUsed     string `json:"proxy_used"`
	Error         string `json:"error"`
	CreatedAt     string `json:"created_at"`
	StartedAt     string `json:"started_at"`
	CompletedAt   string `json:"completed_at"`
	WorkspaceID   string `json:"workspace_id"`
}

// JobDetail is GET /jobs/{id}: the job columns, intent_details parsed (invalid or
// empty JSON -> {}), and the stages last. Marshal with SetEscapeHTML(false) to
// match Python's output.
type JobDetail struct {
	ID            string          `json:"id"`
	Query         string          `json:"query"`
	Intent        string          `json:"intent"`
	IntentDetails json.RawMessage `json:"intent_details"`
	Status        string          `json:"status"`
	Tier          int32           `json:"tier"`
	Attempts      int32           `json:"attempts"`
	MaxAttempts   int32           `json:"max_attempts"`
	LeadsFound    int32           `json:"leads_found"`
	ProxyUsed     string          `json:"proxy_used"`
	Error         string          `json:"error"`
	CreatedAt     string          `json:"created_at"`
	StartedAt     string          `json:"started_at"`
	CompletedAt   string          `json:"completed_at"`
	WorkspaceID   string          `json:"workspace_id"`
	Stages        []Stage         `json:"stages"`
}

// List returns the workspace's jobs, newest first (created_at DESC; id DESC breaks
// ties, which Python leaves arbitrary). status "" means no filter; an unknown status
// matches nothing. limit <= 0 means DefaultListLimit. The result is never nil.
func List(ctx context.Context, tx pgx.Tx, status string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	rows, err := gen.New(tx).CJList(ctx, gen.CJListParams{
		Status:    pgtype.Text{String: status, Valid: status != ""},
		PageLimit: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("collectledger: list: %w", err)
	}
	out := make([]Job, 0, len(rows))
	for _, r := range rows {
		out = append(out, Job(r))
	}
	return out, nil
}

// Detail returns the job with its stages (stage order = id). ErrNotFound when the job
// is missing or in another workspace; ErrNotRenderable when a stored JSON column
// would fail Python's response render (NaN, Infinity, ...).
func Detail(ctx context.Context, tx pgx.Tx, id string) (*JobDetail, error) {
	q := gen.New(tx)
	r, err := q.CJGet(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("collectledger: get: %w", err)
	}
	intent, err := ParseDetails(r.IntentDetails)
	if err != nil {
		return nil, fmt.Errorf("intent_details of %s: %w", id, err)
	}
	stages, err := loadStages(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return &JobDetail{
		ID: r.ID, Query: r.Query, Intent: r.Intent, IntentDetails: intent, Status: r.Status,
		Tier: r.Tier, Attempts: r.Attempts, MaxAttempts: r.MaxAttempts, LeadsFound: r.LeadsFound,
		ProxyUsed: r.ProxyUsed, Error: r.Error, CreatedAt: r.CreatedAt, StartedAt: r.StartedAt,
		CompletedAt: r.CompletedAt, WorkspaceID: r.WorkspaceID, Stages: stages,
	}, nil
}
