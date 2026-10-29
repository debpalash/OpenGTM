package outreachstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// UpsertSchedule writes the non-RLS outreach_schedules mirror row for a
// sequence. A row owned by another workspace is left untouched.
func UpsertSchedule(ctx context.Context, tx pgx.Tx, wsID, seqID string, nextTick time.Time, enabled bool) error {
	err := gen.New(tx).OutreachScheduleUpsert(ctx, gen.OutreachScheduleUpsertParams{
		SequenceID: seqID, WorkspaceID: wsID, NextTickAt: tstz(nextTick), Enabled: enabled,
	})
	if err != nil {
		return fmt.Errorf("outreachstore: upsert schedule: %w", err)
	}
	return nil
}

// DisableSchedule clears enabled on this workspace's mirror row (no-op when absent).
func DisableSchedule(ctx context.Context, tx pgx.Tx, seqID string) error {
	if err := gen.New(tx).OutreachScheduleDisable(ctx, seqID); err != nil {
		return fmt.Errorf("outreachstore: disable schedule: %w", err)
	}
	return nil
}
