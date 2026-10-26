package collectledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Stage is one collection_job_stages row; Details is the parsed JSON column
// (invalid or empty -> {}). Field order is Python's JSON key order.
type Stage struct {
	ID            int64           `json:"id"`
	JobID         string          `json:"job_id"`
	Stage         string          `json:"stage"`
	Status        string          `json:"status"`
	InputCount    int32           `json:"input_count"`
	OutputCount   int32           `json:"output_count"`
	RejectedCount int32           `json:"rejected_count"`
	Details       json.RawMessage `json:"details"`
	StartedAt     string          `json:"started_at"`
	CompletedAt   string          `json:"completed_at"`
}

// Stages is GET /jobs/{id}/stages: ErrNotFound for a missing job (the route checks
// existence first), else the job's stages ordered by id (never nil).
func Stages(ctx context.Context, tx pgx.Tx, id string) ([]Stage, error) {
	q := gen.New(tx)
	if _, err := q.CJGet(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("collectledger: get: %w", err)
	}
	return loadStages(ctx, q, id)
}

func loadStages(ctx context.Context, q *gen.Queries, id string) ([]Stage, error) {
	rows, err := q.CJStages(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("collectledger: stages: %w", err)
	}
	out := make([]Stage, 0, len(rows))
	for _, r := range rows {
		d, err := ParseDetails(r.Details)
		if err != nil {
			return nil, fmt.Errorf("details of stage %d: %w", r.ID, err)
		}
		out = append(out, Stage{
			ID: r.ID, JobID: r.JobID, Stage: r.Stage, Status: r.Status, InputCount: r.InputCount,
			OutputCount: r.OutputCount, RejectedCount: r.RejectedCount, Details: d,
			StartedAt: r.StartedAt, CompletedAt: r.CompletedAt,
		})
	}
	return out, nil
}
