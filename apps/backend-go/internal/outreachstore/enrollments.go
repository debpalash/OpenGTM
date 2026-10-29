package outreachstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
)

// ErrNoEmail is Python's ValueError("no_email"): the snapshot normalizes empty.
var ErrNoEmail = errors.New("no_email")

// Enrollment is one outreach_enrollments row.
type Enrollment struct {
	ID              int
	WorkspaceID     string
	SequenceID      string
	LeadID          int
	ToEmailSnapshot string
	ConsentSource   string
	ConsentAt       *time.Time
	CurrentStep     int
	Status          string
	NextSendAt      *time.Time
	SentCount       int
	SoftBounceCount int
	LastSentAt      *time.Time
	Error           string
}

// EnrollmentDict is _enroll_to_dict (the Python dict key order).
type EnrollmentDict struct {
	ID              int     `json:"id"`
	SequenceID      string  `json:"sequence_id"`
	LeadID          int     `json:"lead_id"`
	ToEmailSnapshot string  `json:"to_email_snapshot"`
	CurrentStep     int     `json:"current_step"`
	Status          string  `json:"status"`
	NextSendAt      *string `json:"next_send_at"`
	SentCount       int     `json:"sent_count"`
	SoftBounceCount int     `json:"soft_bounce_count"`
}

// Dict renders the Python dict shape.
func (e Enrollment) Dict() EnrollmentDict {
	var next *string
	if e.NextSendAt != nil {
		next = isoPy(tstz(*e.NextSendAt))
	}
	return EnrollmentDict{
		ID: e.ID, SequenceID: e.SequenceID, LeadID: e.LeadID, ToEmailSnapshot: e.ToEmailSnapshot,
		CurrentStep: e.CurrentStep, Status: e.Status, NextSendAt: next, SentCount: e.SentCount,
		SoftBounceCount: e.SoftBounceCount,
	}
}

func tp(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}

func toEnrollment(r gen.OutreachEnrollment) Enrollment {
	return Enrollment{
		ID: int(r.ID), WorkspaceID: r.WorkspaceID, SequenceID: r.SequenceID, LeadID: int(r.LeadID),
		ToEmailSnapshot: r.ToEmailSnapshot, ConsentSource: r.ConsentSource.String, ConsentAt: tp(r.ConsentAt),
		CurrentStep: int4Or0(r.CurrentStep), Status: r.Status.String, NextSendAt: tp(r.NextSendAt),
		SentCount: int4Or0(r.SentCount), SoftBounceCount: int4Or0(r.SoftBounceCount),
		LastSentAt: tp(r.LastSentAt), Error: r.Error.String,
	}
}

// Callers must run SequenceExists first: Enroll does not verify that seqID
// belongs to wsID (same as Python).
//
// Enroll inserts one pending enrollment due now. A duplicate (workspace,
// sequence, lead) answers created=false with id 0 and leaves the transaction
// usable. The email is normalized; an empty result is ErrNoEmail. A zero
// consentAt stores NULL.
func Enroll(ctx context.Context, tx pgx.Tx, wsID, seqID string, leadID int, email, consentSource string, consentAt time.Time) (int, bool, error) {
	norm := emailnorm.Normalize(email)
	if norm == "" {
		return 0, false, ErrNoEmail
	}
	p := gen.OutreachEnrollInsertParams{
		WorkspaceID: wsID, SequenceID: seqID, LeadID: int32(leadID), ToEmailSnapshot: norm, ConsentSource: consentSource,
	}
	if !consentAt.IsZero() {
		p.ConsentAt = tstz(consentAt)
	}
	id, err := gen.New(tx).OutreachEnrollInsert(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("outreachstore: enroll: %w", err)
	}
	return int(id), true, nil
}

// GetEnrollment returns nil when the id is unknown in the tx's workspace.
func GetEnrollment(ctx context.Context, tx pgx.Tx, id int) (*Enrollment, error) {
	r, err := gen.New(tx).OutreachEnrollmentGet(ctx, int32(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outreachstore: get enrollment: %w", err)
	}
	e := toEnrollment(r)
	return &e, nil
}

// DueEnrollments lists pending/scheduled enrollments whose next_send_at has
// passed and that have no send row for their current step, oldest first.
// A limit of zero or less returns no rows.
func DueEnrollments(ctx context.Context, tx pgx.Tx, seqID string, limit int) ([]Enrollment, error) {
	if limit <= 0 {
		return []Enrollment{}, nil
	}
	if limit > math.MaxInt32 {
		limit = math.MaxInt32
	}
	rows, err := gen.New(tx).OutreachEnrollmentsDue(ctx, gen.OutreachEnrollmentsDueParams{SequenceID: seqID, RowLimit: int32(limit)})
	if err != nil {
		return nil, fmt.Errorf("outreachstore: due enrollments: %w", err)
	}
	out := make([]Enrollment, 0, len(rows))
	for _, r := range rows {
		out = append(out, toEnrollment(r))
	}
	return out, nil
}

// RescheduleEnrollment moves next_send_at and flips pending to scheduled.
func RescheduleEnrollment(ctx context.Context, tx pgx.Tx, id int, next time.Time) error {
	if err := gen.New(tx).OutreachEnrollmentReschedule(ctx, gen.OutreachEnrollmentRescheduleParams{NextSendAt: tstz(next), ID: int32(id)}); err != nil {
		return fmt.Errorf("outreachstore: reschedule enrollment: %w", err)
	}
	return nil
}

// Advance is advance_enrollment's keyword arguments: Status always applies;
// nil NextSendAt/CurrentStep leave the column; IncrementSent bumps sent_count
// and stamps last_sent_at; a non-empty Error replaces the stored one.
type Advance struct {
	Status        string
	NextSendAt    *time.Time
	CurrentStep   *int
	IncrementSent bool
	Error         string
}

// AdvanceEnrollment applies an Advance; an unknown id is a silent no-op.
func AdvanceEnrollment(ctx context.Context, tx pgx.Tx, id int, a Advance) error {
	p := gen.OutreachEnrollmentAdvanceParams{Status: a.Status, IncrementSent: a.IncrementSent, Error: a.Error, ID: int32(id)}
	if a.NextSendAt != nil {
		p.SetNextSendAt, p.NextSendAt = true, tstz(*a.NextSendAt)
	}
	if a.CurrentStep != nil {
		p.SetCurrentStep, p.CurrentStep = true, int32(*a.CurrentStep)
	}
	if err := gen.New(tx).OutreachEnrollmentAdvance(ctx, p); err != nil {
		return fmt.Errorf("outreachstore: advance enrollment: %w", err)
	}
	return nil
}

// IncrementSoftBounce bumps soft_bounce_count and returns it (0 for an unknown id).
func IncrementSoftBounce(ctx context.Context, tx pgx.Tx, id int) (int, error) {
	n, err := gen.New(tx).OutreachEnrollmentIncSoftBounce(ctx, int32(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("outreachstore: soft bounce: %w", err)
	}
	return int(n.Int32), nil
}
