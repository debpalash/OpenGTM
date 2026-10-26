package outreachstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// InboundIn is record_inbound_processed's keyword arguments.
type InboundIn struct {
	WorkspaceID string
	IMAPUID     string
	UIDValidity string
	// UIDValidityNone mirrors Python's uidvalidity=None: the column stores "" but
	// the synthesized message id reads "uid:None:<uid>" (f-string of None).
	UIDValidityNone bool
	SourceMessageID string
	MatchedSendID   *int32
	Kind            string
	Recipient       string
	Diagnostic      string
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// RecordInboundProcessed inserts the ledger row and reports whether it is new.
// An empty or blank source_message_id becomes uid:<uidvalidity>:<uid> so distinct
// messages never collide on the secondary unique key; recipient is cut to 320
// characters and diagnostic to 500. A conflict on either unique constraint is
// created=false and does not abort the transaction.
func RecordInboundProcessed(ctx context.Context, tx pgx.Tx, in InboundIn) (bool, error) {
	mid := strings.TrimSpace(in.SourceMessageID)
	if mid == "" {
		uv := in.UIDValidity
		if in.UIDValidityNone {
			uv = "None"
		}
		mid = "uid:" + uv + ":" + in.IMAPUID
	}
	p := gen.OutreachInboundRecordParams{
		WorkspaceID: in.WorkspaceID, ImapUid: in.IMAPUID, Uidvalidity: in.UIDValidity, SourceMessageID: mid,
		Kind: in.Kind, Recipient: truncRunes(in.Recipient, 320), Diagnostic: truncRunes(in.Diagnostic, 500),
	}
	if in.MatchedSendID != nil {
		p.MatchedSendID = pgtype.Int4{Int32: *in.MatchedSendID, Valid: true}
	}
	created, err := gen.New(tx).OutreachInboundRecord(ctx, p)
	if err != nil {
		return false, fmt.Errorf("outreachstore: record inbound: %w", err)
	}
	return created, nil
}

// InboundMessageCount is the number of ledger rows in this workspace.
func InboundMessageCount(ctx context.Context, tx pgx.Tx, wsID string) (int, error) {
	n, err := gen.New(tx).OutreachInboundCount(ctx, wsID)
	if err != nil {
		return 0, fmt.Errorf("outreachstore: inbound count: %w", err)
	}
	return int(n), nil
}

// InboundSchedule is the non-RLS outreach_inbound_schedules mirror row.
type InboundSchedule struct {
	WorkspaceID         string
	NextPollAt          *time.Time
	Enabled             bool
	ConsecutiveFailures int
	UIDValidity         string
	LastUID             string
}

// InboundScheduleUpsert: NextPollAt and Enabled always overwrite; a nil pointer
// leaves the other columns as they are.
type InboundScheduleUpsert struct {
	NextPollAt          *time.Time
	Enabled             bool
	ConsecutiveFailures *int
	UIDValidity         *string
	LastUID             *string
}

// GetInboundSchedule returns the workspace's mirror row, or nil when absent.
// The table has no RLS, so this takes the pool, not a tenant transaction.
func GetInboundSchedule(ctx context.Context, db gen.DBTX, wsID string) (*InboundSchedule, error) {
	r, err := gen.New(db).OutreachInboundScheduleGet(ctx, wsID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outreachstore: get inbound schedule: %w", err)
	}
	s := &InboundSchedule{
		WorkspaceID: r.WorkspaceID, Enabled: r.Enabled, ConsecutiveFailures: int(r.ConsecutiveFailures),
		UIDValidity: r.Uidvalidity.String, LastUID: r.LastUid.String,
	}
	if r.NextPollAt.Valid {
		t := r.NextPollAt.Time.UTC()
		s.NextPollAt = &t
	}
	return s, nil
}

// UpsertInboundSchedule writes the mirror row (pool, non-RLS).
func UpsertInboundSchedule(ctx context.Context, db gen.DBTX, wsID string, u InboundScheduleUpsert) error {
	p := gen.OutreachInboundScheduleUpsertParams{WorkspaceID: wsID, Enabled: u.Enabled}
	if u.NextPollAt != nil {
		p.NextPollAt = tstz(*u.NextPollAt)
	}
	if u.ConsecutiveFailures != nil {
		p.SetFailures, p.ConsecutiveFailures = true, int32(*u.ConsecutiveFailures)
	}
	if u.UIDValidity != nil {
		p.SetUidvalidity, p.Uidvalidity = true, *u.UIDValidity
	}
	if u.LastUID != nil {
		p.SetLastUid, p.LastUid = true, *u.LastUID
	}
	if err := gen.New(db).OutreachInboundScheduleUpsert(ctx, p); err != nil {
		return fmt.Errorf("outreachstore: upsert inbound schedule: %w", err)
	}
	return nil
}

// DisableInboundSchedule clears enabled and next_poll_at (no-op when absent).
func DisableInboundSchedule(ctx context.Context, db gen.DBTX, wsID string) error {
	if err := gen.New(db).OutreachInboundScheduleDisable(ctx, wsID); err != nil {
		return fmt.Errorf("outreachstore: disable inbound schedule: %w", err)
	}
	return nil
}
