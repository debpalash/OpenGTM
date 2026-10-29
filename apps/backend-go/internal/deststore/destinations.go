package deststore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

func newUUID4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// CreateInput is the validated body of a destination create. Config and FieldMap
// are JSON objects (nil means {}); the caller has already validated the config.
type CreateInput struct {
	AudienceID, Name, Type string
	Enabled                bool
	Config, FieldMap       []byte
}

// UpdateInput carries the fields of a patch that were sent (nil = omitted).
type UpdateInput struct {
	Name             *string
	Enabled          *bool
	Config, FieldMap []byte
}

func uniqueName(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "uq_audience_destinations_name"
}

// CreateDestination inserts a destination: unique (workspace, audience, name)
// gives ErrDuplicate, an unknown audience ErrAudienceNotFound. The insert runs in
// a savepoint, so a refused create leaves the caller's transaction usable.
func CreateDestination(ctx context.Context, tx pgx.Tx, in CreateInput) (Destination, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return Destination{}, err
	}
	if in.Config == nil {
		in.Config = []byte(`{}`)
	}
	if in.FieldMap == nil {
		in.FieldMap = []byte(`{}`)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Destination{}, fmt.Errorf("deststore: savepoint: %w", err)
	}
	row, err := gen.New(sp).DestInsert(ctx, gen.DestInsertParams{
		ID: newUUID4(), WorkspaceID: ws, AudienceID: in.AudienceID, Name: in.Name, DestinationType: in.Type,
		Enabled: in.Enabled, Config: in.Config, FieldMap: in.FieldMap,
	})
	if err != nil {
		_ = sp.Rollback(ctx)
		var pg *pgconn.PgError
		switch {
		case uniqueName(err):
			return Destination{}, ErrDuplicate
		case errors.As(err, &pg) && pg.Code == "23503":
			return Destination{}, ErrAudienceNotFound
		}
		return Destination{}, fmt.Errorf("deststore: create destination: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return Destination{}, fmt.Errorf("deststore: release savepoint: %w", err)
	}
	return destFrom(row), nil
}

// GetDestination is the workspace-scoped lookup (ErrNotFound for another tenant's id).
func GetDestination(ctx context.Context, tx pgx.Tx, id string) (Destination, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return Destination{}, err
	}
	return getDestination(ctx, gen.New(tx), ws, id)
}

func getDestination(ctx context.Context, q *gen.Queries, ws, id string) (Destination, error) {
	row, err := q.DestGet(ctx, gen.DestGetParams{WorkspaceID: ws, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, ErrNotFound
	}
	if err != nil {
		return Destination{}, fmt.Errorf("deststore: get destination: %w", err)
	}
	return destFrom(row), nil
}

// UpdateDestination applies the sent fields; renaming onto another destination of
// the audience is ErrDuplicate (decision D3: Python answered 500). With nothing sent
// it only reads (SQLAlchemy emits no UPDATE, so updated_at stays).
func UpdateDestination(ctx context.Context, tx pgx.Tx, id string, in UpdateInput) (Destination, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return Destination{}, err
	}
	if in.Name == nil && in.Enabled == nil && in.Config == nil && in.FieldMap == nil {
		return getDestination(ctx, gen.New(tx), ws, id)
	}
	p := gen.DestUpdateParams{WorkspaceID: ws, ID: id, Config: in.Config, FieldMap: in.FieldMap}
	p.Name = pgText(in.Name)
	if in.Enabled != nil {
		p.Enabled = pgtype.Bool{Bool: *in.Enabled, Valid: true}
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Destination{}, fmt.Errorf("deststore: savepoint: %w", err)
	}
	row, err := gen.New(sp).DestUpdate(ctx, p)
	if err != nil {
		_ = sp.Rollback(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return Destination{}, ErrNotFound
		case uniqueName(err):
			return Destination{}, ErrDuplicate
		}
		return Destination{}, fmt.Errorf("deststore: update destination: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return Destination{}, fmt.Errorf("deststore: release savepoint: %w", err)
	}
	return destFrom(row), nil
}

// ListDestinationsWithLatestRun lists the workspace's destinations (one audience
// when audienceID is non-empty), newest updated first, each with its latest run
// (rank 1 by created_at DESC, id DESC).
func ListDestinationsWithLatestRun(ctx context.Context, tx pgx.Tx, audienceID string) ([]DestinationWithRun, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := gen.New(tx).DestListWithLatestRun(ctx, gen.DestListWithLatestRunParams{WorkspaceID: ws, AudienceID: audienceID})
	if err != nil {
		return nil, fmt.Errorf("deststore: list destinations: %w", err)
	}
	out := make([]DestinationWithRun, 0, len(rows))
	for _, r := range rows {
		item := DestinationWithRun{Destination: destFrom(gen.AudienceDestination{
			ID: r.ID, WorkspaceID: r.WorkspaceID, AudienceID: r.AudienceID, Name: r.Name, DestinationType: r.DestinationType,
			Enabled: r.Enabled, Config: r.Config, FieldMap: r.FieldMap, HealthStatus: r.HealthStatus, LastError: r.LastError,
			LastSuccessAt: r.LastSuccessAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})}
		if r.RunID.Valid {
			run := runFrom(gen.DestinationRun{
				ID: r.RunID.String, WorkspaceID: r.RunWorkspaceID.String, DestinationID: r.RunDestinationID.String,
				Status: r.RunStatus.String, RequestedBy: r.RunRequestedBy, Attempted: r.RunAttempted.Int32,
				Succeeded: r.RunSucceeded.Int32, Failed: r.RunFailed.Int32, Skipped: r.RunSkipped.Int32,
				Error: r.RunError, StartedAt: r.RunStartedAt, FinishedAt: r.RunFinishedAt, CreatedAt: r.RunCreatedAt,
			})
			item.LatestRun = &run
		}
		out = append(out, item)
	}
	return out, nil
}

// DeleteDestination revokes the destination's live inbound tokens, then deletes it
// (runs and deliveries cascade), as the Python route does. ErrNotFound for an
// unknown or foreign id (nothing is revoked then).
func DeleteDestination(ctx context.Context, tx pgx.Tx, id string) error {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return err
	}
	q := gen.New(tx)
	if _, err := getDestination(ctx, q, ws, id); err != nil {
		return err
	}
	if _, err := q.DestRevokeInboundTokens(ctx, gen.DestRevokeInboundTokensParams{WorkspaceID: ws, DestinationID: id}); err != nil {
		return fmt.Errorf("deststore: revoke inbound tokens: %w", err)
	}
	n, err := q.DestDelete(ctx, gen.DestDeleteParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return fmt.Errorf("deststore: delete destination: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
