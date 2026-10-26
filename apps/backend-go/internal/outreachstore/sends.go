// Package outreachstore: outreach send ledger and stats (plan 09 task 13).
// Port of the "Sends" section of legacy PgOutreachStore plus sending._finalize_send
// and sending._record_terminal. Tenant statements run on the caller's
// tenancy.InWorkspace transaction; the idempotency key uq_outreach_send_idem is
// the at-most-once marker for SMTP sends.
package outreachstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
)

// sendsNow is the clock for the sent-count windows (swapped in tests).
var sendsNow = func() time.Time { return time.Now().UTC() }

// Send is a full outreach_sends row (nullable columns map to Go zero values,
// nullable timestamps to pointers). Python get_send_by_idem returns the ORM row.
type Send struct {
	ID             int32
	WorkspaceID    string
	SequenceID     string
	EnrollmentID   *int32
	LeadID         *int32
	StepNumber     int32
	ToEmail        string
	Subject        string
	Status         string
	SkipReason     string
	MessageID      string
	IdempotencyKey string
	ChargedUSD     float64
	Migrated       bool
	Error          string
	SentAt         *time.Time
	OpenedAt       *time.Time
	RepliedAt      *time.Time
	BouncedAt      *time.Time
	CreatedAt      *time.Time
	UpdatedAt      *time.Time
}

// SendRef mirrors Python PgOutreachStore._send_to_dict (charged_usd rounded to
// 4 decimals).
type SendRef struct {
	ID           int32
	SequenceID   string
	EnrollmentID *int32
	LeadID       *int32
	StepNumber   int32
	ToEmail      string
	Subject      string
	Status       string
	SkipReason   string
	MessageID    string
	ChargedUSD   float64
	Migrated     bool
	Error        string
	SentAt       *time.Time
	CreatedAt    *time.Time
}

// GlobalRef is the routing tuple returned by the SECURITY DEFINER lookup.
type GlobalRef struct {
	WorkspaceID  string
	SendID       int32
	SequenceID   string // "" when the send has no sequence
	EnrollmentID *int32
	ToEmail      string
}

// SendIn carries the columns written when a send row is first created.
type SendIn struct {
	WorkspaceID    string
	SequenceID     string // "" stores NULL
	EnrollmentID   *int32
	LeadID         *int32
	StepNumber     int32
	ToEmail        string
	Subject        string
	IdempotencyKey string
}

// Finalize lists the fields a FinalizeSend may change. Zero values mean "leave
// unchanged" exactly like Python _finalize_send (truthiness checks).
type Finalize struct {
	Status     string
	SkipReason string
	MessageID  string
	Error      string
	SentAt     *time.Time
	BouncedAt  *time.Time
	ChargedUSD *float64
}

func sendsText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func sendsTs(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func sendsTsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func sendsInt4(p *int32) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *p, Valid: true}
}

func sendsInt4Ptr(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	x := v.Int32
	return &x
}

// GetSendByIdem returns the workspace's send with this idempotency key, or nil.
func GetSendByIdem(ctx context.Context, tx pgx.Tx, wsID, idem string) (*Send, error) {
	r, err := gen.New(tx).OutreachSendGetByIdem(ctx, gen.OutreachSendGetByIdemParams{WorkspaceID: wsID, IdempotencyKey: idem})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Send{
		ID: r.ID, WorkspaceID: r.WorkspaceID, SequenceID: r.SequenceID.String,
		EnrollmentID: sendsInt4Ptr(r.EnrollmentID), LeadID: sendsInt4Ptr(r.LeadID),
		StepNumber: r.StepNumber.Int32, ToEmail: r.ToEmail, Subject: r.Subject.String,
		Status: r.Status.String, SkipReason: r.SkipReason.String, MessageID: r.MessageID.String,
		IdempotencyKey: r.IdempotencyKey, ChargedUSD: r.ChargedUsd.Float64, Migrated: r.Migrated.Bool,
		Error: r.Error.String, SentAt: sendsTsPtr(r.SentAt), OpenedAt: sendsTsPtr(r.OpenedAt),
		RepliedAt: sendsTsPtr(r.RepliedAt), BouncedAt: sendsTsPtr(r.BouncedAt),
		CreatedAt: sendsTsPtr(r.CreatedAt), UpdatedAt: sendsTsPtr(r.UpdatedAt),
	}, nil
}

// InsertInFlight writes the pre-send in_flight marker (step 8 of _do_send). A
// conflict on uq_outreach_send_idem (concurrent or replayed claim) returns
// created=false with no error and leaves the transaction usable. The caller must
// commit this transaction BEFORE dialing SMTP.
func InsertInFlight(ctx context.Context, tx pgx.Tx, in SendIn) (bool, error) {
	n, err := gen.New(tx).OutreachSendInsertInFlight(ctx, gen.OutreachSendInsertInFlightParams{
		WorkspaceID: in.WorkspaceID, SequenceID: sendsText(in.SequenceID),
		EnrollmentID: sendsInt4(in.EnrollmentID), LeadID: sendsInt4(in.LeadID),
		StepNumber: in.StepNumber, ToEmail: in.ToEmail, Subject: in.Subject,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// InsertTerminal records a terminal row for a skip or failure that never reached
// in_flight. An existing idempotency key is left untouched (_record_terminal).
// An empty recipient is stored as "unknown@unknown" like Python.
func InsertTerminal(ctx context.Context, tx pgx.Tx, in SendIn, status, skipReason, errText string) error {
	to := in.ToEmail
	if to == "" {
		to = "unknown@unknown"
	}
	_, err := gen.New(tx).OutreachSendInsertTerminal(ctx, gen.OutreachSendInsertTerminalParams{
		WorkspaceID: in.WorkspaceID, SequenceID: sendsText(in.SequenceID),
		EnrollmentID: sendsInt4(in.EnrollmentID), LeadID: sendsInt4(in.LeadID),
		StepNumber: in.StepNumber, ToEmail: to, Subject: in.Subject, Status: status,
		SkipReason: sendsText(skipReason), Error: errText, IdempotencyKey: in.IdempotencyKey,
	})
	return err
}

// FinalizeSend updates only the fields set in f. An unknown key is a no-op.
func FinalizeSend(ctx context.Context, tx pgx.Tx, wsID, idem string, f Finalize) error {
	_, err := gen.New(tx).OutreachSendFinalize(ctx, gen.OutreachSendFinalizeParams{
		Status: f.Status, SkipReason: f.SkipReason, MessageID: f.MessageID, Error: f.Error,
		SentAt: sendsTs(f.SentAt), BouncedAt: sendsTs(f.BouncedAt),
		ChargedUsd:  pgtype.Float8{Float64: derefFloat(f.ChargedUSD), Valid: f.ChargedUSD != nil},
		WorkspaceID: wsID, IdempotencyKey: idem,
	})
	return err
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func sendsRound4(v float64) float64 {
	// Python round(x, 4) on values stored as double precision.
	const k = 10000.0
	if v >= 0 {
		return float64(int64(v*k+0.5)) / k
	}
	return -float64(int64(-v*k+0.5)) / k
}

// GetSendByMessageID returns the most recent send in this workspace with this
// Message-ID (trimmed; blank returns nil). Workspace-scoped, no cross-tenant read.
func GetSendByMessageID(ctx context.Context, tx pgx.Tx, wsID, msgID string) (*SendRef, error) {
	mid := strings.TrimSpace(msgID)
	if mid == "" {
		return nil, nil
	}
	r, err := gen.New(tx).OutreachSendGetByMessageID(ctx, gen.OutreachSendGetByMessageIDParams{WorkspaceID: wsID, MessageID: mid})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &SendRef{ID: r.ID, SequenceID: r.SequenceID.String, EnrollmentID: sendsInt4Ptr(r.EnrollmentID),
		LeadID: sendsInt4Ptr(r.LeadID), StepNumber: r.StepNumber.Int32, ToEmail: r.ToEmail,
		Subject: r.Subject.String, Status: r.Status.String, SkipReason: r.SkipReason.String,
		MessageID: r.MessageID.String, ChargedUSD: sendsRound4(r.ChargedUsd.Float64), Migrated: r.Migrated.Bool,
		Error: r.Error.String, SentAt: sendsTsPtr(r.SentAt), CreatedAt: sendsTsPtr(r.CreatedAt)}, nil
}

// LookupSendAnyWorkspace resolves a Message-ID across workspaces via the
// SECURITY DEFINER function outreach_send_by_message_id. It is the only
// non-scoped statement in the package: it runs on the pool with no tenant GUC
// and returns only the routing tuple. Unknown or blank ids return nil.
func LookupSendAnyWorkspace(ctx context.Context, pool *pgxpool.Pool, msgID string) (*GlobalRef, error) {
	mid := strings.TrimSpace(msgID)
	if mid == "" {
		return nil, nil
	}
	r, err := gen.New(pool).OutreachSendLookupAnyWorkspace(ctx, mid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ref := &GlobalRef{WorkspaceID: r.WorkspaceID, SendID: r.SendID, SequenceID: r.SequenceID, ToEmail: r.ToEmail}
	if r.HasEnrollment {
		e := r.EnrollmentID
		ref.EnrollmentID = &e
	}
	return ref, nil
}

// GetRecentSendTo returns the most recent send to the canonical recipient,
// preferring sent/in_flight rows (ledger annotation only).
func GetRecentSendTo(ctx context.Context, tx pgx.Tx, wsID, email string) (*SendRef, error) {
	canonical := emailnorm.Normalize(email)
	if canonical == "" {
		return nil, nil
	}
	q := gen.New(tx)
	live, err := q.OutreachSendRecentLive(ctx, gen.OutreachSendRecentLiveParams{WorkspaceID: wsID, ToEmail: canonical})
	if err == nil {
		return refFromLive(live), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	any, err := q.OutreachSendRecentAny(ctx, gen.OutreachSendRecentAnyParams{WorkspaceID: wsID, ToEmail: canonical})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return refFromAny(any), nil
}

func refFromLive(r gen.OutreachSendRecentLiveRow) *SendRef {
	return &SendRef{ID: r.ID, SequenceID: r.SequenceID.String, EnrollmentID: sendsInt4Ptr(r.EnrollmentID),
		LeadID: sendsInt4Ptr(r.LeadID), StepNumber: r.StepNumber.Int32, ToEmail: r.ToEmail,
		Subject: r.Subject.String, Status: r.Status.String, SkipReason: r.SkipReason.String,
		MessageID: r.MessageID.String, ChargedUSD: sendsRound4(r.ChargedUsd.Float64), Migrated: r.Migrated.Bool,
		Error: r.Error.String, SentAt: sendsTsPtr(r.SentAt), CreatedAt: sendsTsPtr(r.CreatedAt)}
}

func refFromAny(r gen.OutreachSendRecentAnyRow) *SendRef {
	return refFromLive(gen.OutreachSendRecentLiveRow(r))
}

// MarkSendBounced sets status=bounced and bounced_at (first value kept); error is
// truncated to 500 chars and only written when non-empty. False when the id is
// not in this workspace.
func MarkSendBounced(ctx context.Context, tx pgx.Tx, wsID string, sendID int32, errText string) (bool, error) {
	n, err := gen.New(tx).OutreachSendMarkBounced(ctx, gen.OutreachSendMarkBouncedParams{WorkspaceID: wsID, ID: sendID, Error: errText})
	return n > 0, err
}

// ListSends returns sends newest first, optionally for one sequence.
func ListSends(ctx context.Context, tx pgx.Tx, wsID, seqID string, limit int) ([]SendRef, error) {
	rows, err := gen.New(tx).OutreachSendList(ctx, gen.OutreachSendListParams{WorkspaceID: wsID, SequenceID: seqID, RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]SendRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, *refFromLive(gen.OutreachSendRecentLiveRow(r)))
	}
	return out, nil
}

// SentCountLastHour counts status='sent' rows with sent_at in the last hour
// (cutoff inclusive), workspace-wide.
func SentCountLastHour(ctx context.Context, tx pgx.Tx, wsID string) (int, error) {
	n, err := gen.New(tx).OutreachSendCountSentSince(ctx, gen.OutreachSendCountSentSinceParams{
		WorkspaceID: wsID, Cutoff: pgtype.Timestamptz{Time: sendsNow().Add(-time.Hour), Valid: true}})
	return int(n), err
}

// SentCountToday counts a sequence's sent rows in the trailing 24 hours. Python
// sent_count_today uses now-1day (a rolling window, not a UTC-midnight boundary);
// this matches it exactly.
func SentCountToday(ctx context.Context, tx pgx.Tx, wsID, seqID string) (int, error) {
	n, err := gen.New(tx).OutreachSendCountSeqSentSince(ctx, gen.OutreachSendCountSeqSentSinceParams{
		WorkspaceID: wsID, SequenceID: seqID, Cutoff: pgtype.Timestamptz{Time: sendsNow().Add(-24 * time.Hour), Valid: true}})
	return int(n), err
}

var sendsStatKeys = []string{"pending", "scheduled", "sent", "opened", "replied",
	"bounced", "failed", "skipped", "suppressed", "completed"}

// SequenceStats mirrors PgOutreachStore.get_sequence_stats: zero-filled
// enrollment status counts, total (sum over every status), emails_sent, and the
// breaker counters (bounce_count, complaint_count, auto_paused 0/1) only when
// the sequence exists.
func SequenceStats(ctx context.Context, tx pgx.Tx, wsID, seqID string) (map[string]int, error) {
	q := gen.New(tx)
	rows, err := q.OutreachSendEnrollmentStatusCounts(ctx, gen.OutreachSendEnrollmentStatusCountsParams{WorkspaceID: wsID, SequenceID: seqID})
	if err != nil {
		return nil, err
	}
	by := map[string]int{}
	total := 0
	for _, r := range rows {
		by[r.Status] = int(r.N)
		total += int(r.N)
	}
	stats := map[string]int{}
	for _, k := range sendsStatKeys {
		stats[k] = by[k]
	}
	stats["total"] = total
	sent, err := q.OutreachSendCountSeqSent(ctx, gen.OutreachSendCountSeqSentParams{WorkspaceID: wsID, SequenceID: seqID})
	if err != nil {
		return nil, err
	}
	stats["emails_sent"] = int(sent)
	c, err := q.OutreachSendSequenceCounters(ctx, gen.OutreachSendSequenceCountersParams{WorkspaceID: wsID, SequenceID: seqID})
	if err == nil {
		stats["bounce_count"] = int(c.BounceCount)
		stats["complaint_count"] = int(c.ComplaintCount)
		if c.AutoPaused {
			stats["auto_paused"] = 1
		} else {
			stats["auto_paused"] = 0
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return stats, nil
}
