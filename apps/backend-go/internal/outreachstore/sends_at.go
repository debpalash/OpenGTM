package outreachstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// SentCountLastHourAt is SentCountLastHour against an explicit clock (the send
// handler's injected now), so the window is not tied to the wall clock.
func SentCountLastHourAt(ctx context.Context, tx pgx.Tx, wsID string, now time.Time) (int, error) {
	n, err := gen.New(tx).OutreachSendCountSentSince(ctx, gen.OutreachSendCountSentSinceParams{
		WorkspaceID: wsID, Cutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}})
	return int(n), err
}

// SentCountTodayAt is SentCountToday (rolling 24h) against an explicit clock.
func SentCountTodayAt(ctx context.Context, tx pgx.Tx, wsID, seqID string, now time.Time) (int, error) {
	n, err := gen.New(tx).OutreachSendCountSeqSentSince(ctx, gen.OutreachSendCountSeqSentSinceParams{
		WorkspaceID: wsID, SequenceID: seqID, Cutoff: pgtype.Timestamptz{Time: now.Add(-24 * time.Hour), Valid: true}})
	return int(n), err
}
