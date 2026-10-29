package deststore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

const maxDeliveryError = 1000

// ListDeliveries is a run's ledger by id ascending. status "" is no filter, afterID
// nil is from the start; the caller validates the status vocabulary and limit.
// ErrRunNotFound when the run is not in the workspace.
func ListDeliveries(ctx context.Context, tx pgx.Tx, runID, status string, afterID *int, limit int) ([]Delivery, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1<<20 {
		return nil, fmt.Errorf("deststore: bad limit %d", limit)
	}
	g := gen.New(tx)
	if _, err := g.DestRunGet(ctx, gen.DestRunGetParams{WorkspaceID: ws, ID: runID}); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunNotFound
	} else if err != nil {
		return nil, fmt.Errorf("deststore: get run: %w", err)
	}
	p := gen.DestDeliveriesListParams{WorkspaceID: ws, RunID: runID, RowLimit: int32(limit)}
	if status != "" {
		p.Status = pgtype.Text{String: status, Valid: true}
	}
	if afterID != nil {
		// ids are int4: anything outside the range compares as before/after every row
		v := int32(math.Max(math.MinInt32, math.Min(math.MaxInt32, float64(*afterID))))
		p.AfterID = pgtype.Int4{Int32: v, Valid: true}
	}
	rows, err := g.DestDeliveriesList(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("deststore: list deliveries: %w", err)
	}
	out := make([]Delivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, deliveryFrom(r))
	}
	return out, nil
}

// UpsertDeliveryInput is one ledger entry of a sync, keyed by IdempotencyKey.
// Attempts is added to the row's count (1 for an immediate delivery, 0 for a batch
// deferred to later or a skip); Summary is only written when non-nil.
type UpsertDeliveryInput struct {
	RunID, DestinationID      string
	LeadID                    int
	Operation, IdempotencyKey string
	Fingerprint               string
	Payload                   json.RawMessage
	Status                    string
	Attempts                  int
	Summary                   *string
}

// UpsertOutcome says what UpsertDelivery did.
type UpsertOutcome int

const (
	// Inserted: a new ledger row.
	Inserted UpsertOutcome = iota + 1
	// Revived: a non-success row of the same key moved to this run (error cleared,
	// payload replaced, status as given, attempts added).
	Revived
	// AlreadySucceeded: the key already delivered; the row is returned untouched
	// and the sync counts it as skipped.
	AlreadySucceeded
)

// UpsertDelivery is the "prior or new DestinationDelivery" step of the sync.
func UpsertDelivery(ctx context.Context, tx pgx.Tx, in UpsertDeliveryInput) (Delivery, UpsertOutcome, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return Delivery{}, 0, err
	}
	if in.LeadID < math.MinInt32 || in.LeadID > math.MaxInt32 || in.Attempts < 0 || in.Attempts > math.MaxInt32 {
		return Delivery{}, 0, errors.New("deststore: delivery numbers out of range")
	}
	if len(in.Payload) == 0 {
		in.Payload = json.RawMessage(`{}`)
	}
	q := gen.New(tx)
	row, err := q.DestDeliveryUpsert(ctx, gen.DestDeliveryUpsertParams{
		WorkspaceID: ws, RunID: in.RunID, DestinationID: in.DestinationID, LeadID: int32(in.LeadID), Operation: in.Operation,
		IdempotencyKey: in.IdempotencyKey, PayloadFingerprint: in.Fingerprint, Payload: in.Payload, Status: in.Status,
		Attempts: int32(in.Attempts), Summary: pgText(in.Summary),
	})
	if errors.Is(err, pgx.ErrNoRows) { // the conflicting row is a success: left as is
		prior, err := q.DestDeliveryByKey(ctx, gen.DestDeliveryByKeyParams{WorkspaceID: ws, IdempotencyKey: in.IdempotencyKey})
		if err != nil {
			return Delivery{}, 0, fmt.Errorf("deststore: read delivered row: %w", err)
		}
		return deliveryFrom(prior), AlreadySucceeded, nil
	}
	if err != nil {
		return Delivery{}, 0, fmt.Errorf("deststore: upsert delivery: %w", err)
	}
	d := deliveryFrom(gen.DestinationDelivery{
		ID: row.ID, WorkspaceID: row.WorkspaceID, RunID: row.RunID, DestinationID: row.DestinationID, LeadID: row.LeadID,
		Operation: row.Operation, IdempotencyKey: row.IdempotencyKey, PayloadFingerprint: row.PayloadFingerprint,
		Status: row.Status, Attempts: row.Attempts, ExternalID: row.ExternalID, Summary: row.Summary, Error: row.Error,
		DeliveredAt: row.DeliveredAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Payload: row.Payload,
	})
	if row.Inserted {
		return d, Inserted, nil
	}
	return d, Revived, nil
}

// SettleInput is the outcome of one delivery. Error is cut to 1000 characters and
// stored as NULL when empty; Delivered stamps delivered_at (success).
type SettleInput struct {
	Status     string
	Summary    *string
	Error      string
	ExternalID *string
	Delivered  bool
}

// SettleDelivery records a delivery result (status, summary, error, external id).
func SettleDelivery(ctx context.Context, tx pgx.Tx, id int, in SettleInput) (Delivery, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return Delivery{}, err
	}
	if id < math.MinInt32 || id > math.MaxInt32 {
		return Delivery{}, ErrNotFound
	}
	p := gen.DestDeliverySettleParams{
		WorkspaceID: ws, ID: int32(id), Status: in.Status, Summary: pgText(in.Summary),
		ExternalID: pgText(in.ExternalID), Delivered: in.Delivered,
	}
	if e := truncRunes(in.Error, maxDeliveryError); e != "" {
		p.Error = pgtype.Text{String: e, Valid: true}
	}
	row, err := gen.New(tx).DestDeliverySettle(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, ErrNotFound
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("deststore: settle delivery: %w", err)
	}
	return deliveryFrom(row), nil
}
