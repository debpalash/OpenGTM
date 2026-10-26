package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govaudit"
)

// Run is a retention_runs row as RetentionRun.to_api renders it: id, status, requested_by,
// policy_snapshot, deleted_counts, error, started_at, finished_at, created_at. The timestamp
// columns are naive and render without an offset.
type Run struct {
	ID             string
	Status         string
	RequestedBy    *string
	PolicySnapshot json.RawMessage
	DeletedCounts  json.RawMessage
	Error          *string
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CreatedAt      time.Time
}

func (r Run) MarshalJSON() ([]byte, error) {
	iso := func(t *time.Time) *string {
		if t == nil {
			return nil
		}
		s := isoformat(*t)
		return &s
	}
	obj := func(raw json.RawMessage) json.RawMessage {
		if len(raw) == 0 || string(raw) == "null" {
			return json.RawMessage("{}")
		}
		return raw
	}
	return json.Marshal(struct {
		ID             string          `json:"id"`
		Status         string          `json:"status"`
		RequestedBy    *string         `json:"requested_by"`
		PolicySnapshot json.RawMessage `json:"policy_snapshot"`
		DeletedCounts  json.RawMessage `json:"deleted_counts"`
		Error          *string         `json:"error"`
		StartedAt      *string         `json:"started_at"`
		FinishedAt     *string         `json:"finished_at"`
		CreatedAt      string          `json:"created_at"`
	}{r.ID, r.Status, r.RequestedBy, obj(r.PolicySnapshot), obj(r.DeletedCounts), r.Error, iso(r.StartedAt), iso(r.FinishedAt), isoformat(r.CreatedAt)})
}

func runFrom(id, status string, by pgtype.Text, snap, counts []byte, errText pgtype.Text, started, finished, created pgtype.Timestamp) Run {
	r := Run{ID: id, Status: status, PolicySnapshot: snap, DeletedCounts: counts, CreatedAt: created.Time}
	if by.Valid {
		r.RequestedBy = &by.String
	}
	if errText.Valid {
		r.Error = &errText.String
	}
	if started.Valid {
		t := started.Time
		r.StartedAt = &t
	}
	if finished.Valid {
		t := finished.Time
		r.FinishedAt = &t
	}
	return r
}

// StoredDays normalizes a stored retention_days document the way normalized_days(policy.retention_days)
// does: defaults fill the gaps, every value is range checked. A stored value that is not valid is an error.
func StoredDays(raw json.RawMessage) (Days, error) {
	var in map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&in); err != nil {
			return nil, fmt.Errorf("retention: stored days: %w", err)
		}
	}
	return NormalizeDays(in)
}

// RequestRun records an operator-requested run and its retention_enforce job in the caller's
// transaction (inside tenancy.InWorkspace), so a run never exists without its job: if either insert
// fails the caller rolls both back. The policy is read FOR SHARE first, so a concurrent legal-hold
// change either lands before (the run is refused) or waits for this transaction. The run is pending,
// requested_by is the user id as text and policy_snapshot the normalized days; the job is due now
// with fire_key retention-run:<id> and payload {workspace_id, run_id}. ErrNoPolicy / ErrLegalHold
// refuse the request.
func RequestRun(ctx context.Context, tx pgx.Tx, ws string, userID int32, executor string) (Run, error) {
	if executor != "go" && executor != "python" {
		return Run{}, fmt.Errorf("retention: executor %q must be go or python", executor)
	}
	q := gen.New(tx)
	row, err := q.RetPolicyLockShared(ctx, ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNoPolicy
	}
	if err != nil {
		return Run{}, fmt.Errorf("retention: read policy: %w", err)
	}
	if row.LegalHold {
		return Run{}, ErrLegalHold
	}
	snap, err := snapshotOf(&Policy{RetentionDays: row.RetentionDays})
	if err != nil {
		return Run{}, err
	}
	r, err := q.RetRunInsertRequested(ctx, gen.RetRunInsertRequestedParams{
		WorkspaceID: ws, RequestedBy: fmt.Sprint(userID), PolicySnapshot: snap})
	if err != nil {
		return Run{}, fmt.Errorf("retention: insert run: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		WorkspaceID string `json:"workspace_id"`
		RunID       string `json:"run_id"`
	}{ws, r.ID}); err != nil {
		return Run{}, err
	}
	if _, err := q.RetInsertRunJob(ctx, gen.RetInsertRunJobParams{
		Executor: executor, Payload: bytes.TrimSpace(buf.Bytes()), WorkspaceID: ws, FireKey: "retention-run:" + r.ID}); err != nil {
		return Run{}, fmt.Errorf("retention: enqueue run: %w", err)
	}
	return runFrom(r.ID, r.Status, r.RequestedBy, r.PolicySnapshot, r.DeletedCounts, r.Error, r.StartedAt, r.FinishedAt, r.CreatedAt), nil
}

// Refusals of RequestRun, with the detail Python answers (409).
var (
	ErrNoPolicy  = errors.New("Configure a retention policy first")
	ErrLegalHold = errors.New("Legal hold blocks retention enforcement")
)

// RunPage is the runs list body: runs, limit, offset, has_more, next_cursor. Offset is null when a
// cursor was given.
type RunPage struct {
	Runs       []Run   `json:"runs"`
	Limit      int     `json:"limit"`
	Offset     *int    `json:"offset"`
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
}

// ListRuns pages the workspace's runs newest first (created_at, id). limit is 1..200 and offset
// 0..100000 (validated by the caller); a non-empty cursor wins over offset. A malformed cursor is a
// *govaudit.CursorError "invalid retention run cursor".
func ListRuns(ctx context.Context, tx pgx.Tx, ws string, limit, offset int, cursor string) (RunPage, error) {
	p := gen.RetRunsListParams{WorkspaceID: ws, RowLimit: int32(limit + 1), RowOffset: int32(offset)}
	if cursor != "" {
		at, id, cerr := govaudit.DecodeCursor(cursor, "retention run")
		if cerr != nil {
			return RunPage{}, cerr
		}
		p.UseCursor, p.CursorID, p.RowOffset = true, id, 0
		p.CursorCreated = pgtype.Timestamptz{Time: at.UTC(), Valid: true}
	}
	rows, err := gen.New(tx).RetRunsList(ctx, p)
	if err != nil {
		return RunPage{}, fmt.Errorf("retention: list runs: %w", err)
	}
	pg := RunPage{Runs: make([]Run, 0, min(len(rows), limit)), Limit: limit, HasMore: len(rows) > limit}
	if !p.UseCursor {
		pg.Offset = &offset
	}
	for i, r := range rows {
		if i == limit {
			break
		}
		pg.Runs = append(pg.Runs, runFrom(r.ID, r.Status, r.RequestedBy, r.PolicySnapshot, r.DeletedCounts, r.Error, r.StartedAt, r.FinishedAt, r.CreatedAt))
	}
	if pg.HasMore {
		last := pg.Runs[len(pg.Runs)-1]
		c := govaudit.EncodeCursor(last.CreatedAt, last.ID)
		pg.NextCursor = &c
	}
	return pg, nil
}
