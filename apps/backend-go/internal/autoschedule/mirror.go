package autoschedule

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

// MirrorSet is mirror_set/_mirror_upsert: upsert the non-RLS scheduled_triggers
// row. A nil next stores NULL (due at the next bootstrap). It joins the
// caller's transaction.
func MirrorSet(ctx context.Context, tx pgx.Tx, triggerID, ws string, next *time.Time, enabled bool) error {
	p := gen.UpsertScheduledTriggerParams{TriggerID: triggerID, WorkspaceID: ws, Enabled: enabled}
	if next != nil {
		p.NextRunAt = tstz(*next)
	}
	if err := gen.New(tx).UpsertScheduledTrigger(ctx, p); err != nil {
		return fmt.Errorf("autoschedule: mirror set: %w", err)
	}
	return nil
}

// MirrorDelete is mirror_delete.
func MirrorDelete(ctx context.Context, tx pgx.Tx, triggerID string) error {
	if err := gen.New(tx).DeleteScheduledTrigger(ctx, triggerID); err != nil {
		return fmt.Errorf("autoschedule: mirror delete: %w", err)
	}
	return nil
}
