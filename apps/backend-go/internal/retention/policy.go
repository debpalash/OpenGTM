package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Policy is a retention_policies row. Its timestamps are naive column values
// (located in time.UTC, no offset) and render without a suffix like Python.
type Policy struct {
	WorkspaceID   string
	Enabled       bool
	LegalHold     bool
	RetentionDays json.RawMessage // the stored JSON object, key order preserved
	NextRunAt     *time.Time
	UpdatedBy     *int32
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// MarshalJSON is RetentionPolicy.to_api: workspace_id, enabled, legal_hold,
// retention_days, next_run_at, updated_by, created_at, updated_at.
func (p Policy) MarshalJSON() ([]byte, error) {
	days := p.RetentionDays
	if len(days) == 0 || string(days) == "null" {
		days = json.RawMessage("{}")
	}
	var next *string
	if p.NextRunAt != nil {
		s := isoformat(*p.NextRunAt)
		next = &s
	}
	return json.Marshal(struct {
		WorkspaceID   string          `json:"workspace_id"`
		Enabled       bool            `json:"enabled"`
		LegalHold     bool            `json:"legal_hold"`
		RetentionDays json.RawMessage `json:"retention_days"`
		NextRunAt     *string         `json:"next_run_at"`
		UpdatedBy     *int32          `json:"updated_by"`
		CreatedAt     string          `json:"created_at"`
		UpdatedAt     string          `json:"updated_at"`
	}{p.WorkspaceID, p.Enabled, p.LegalHold, days, next, p.UpdatedBy, isoformat(p.CreatedAt), isoformat(p.UpdatedAt)})
}

// isoformat renders a naive column time like datetime.isoformat().
func isoformat(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

func fromRow(r gen.RetentionPolicy) Policy {
	p := Policy{WorkspaceID: r.WorkspaceID, Enabled: r.Enabled, LegalHold: r.LegalHold, RetentionDays: r.RetentionDays,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}
	if r.NextRunAt.Valid {
		t := r.NextRunAt.Time
		p.NextRunAt = &t
	}
	if r.UpdatedBy.Valid {
		v := r.UpdatedBy.Int32
		p.UpdatedBy = &v
	}
	return p
}

// GetPolicy returns the workspace's policy, or nil when none exists. tx must be
// inside tenancy.InWorkspace.
func GetPolicy(ctx context.Context, tx pgx.Tx, ws string) (*Policy, error) {
	r, err := gen.New(tx).RetGetPolicy(ctx, ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("retention: get policy: %w", err)
	}
	p := fromRow(r)
	return &p, nil
}

// UpsertPolicy stores enabled, legal hold and the normalized days (full
// DefaultDays-ordered dict, as Python stores it) and updated_by, creating the
// row when absent. next_run_at is untouched: call SchedulePolicy next in the
// same transaction. An invalid days map is refused before anything is written.
func UpsertPolicy(ctx context.Context, tx pgx.Tx, ws string, enabled, hold bool, days map[string]int, by int32) (Policy, error) {
	norm, err := normalizeInts(days)
	if err != nil {
		return Policy{}, err
	}
	body, err := json.Marshal(norm)
	if err != nil {
		return Policy{}, err
	}
	r, err := gen.New(tx).RetUpsertPolicy(ctx, gen.RetUpsertPolicyParams{
		WorkspaceID: ws, Enabled: enabled, LegalHold: hold, RetentionDays: body, UpdatedBy: pgtype.Int4{Int32: by, Valid: true},
	})
	if err != nil {
		return Policy{}, fmt.Errorf("retention: upsert policy: %w", err)
	}
	return fromRow(r), nil
}
