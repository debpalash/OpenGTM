package outreachstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Thresholds are the circuit-breaker and soft-bounce settings
// (OUTREACH_CIRCUIT_MIN_SENDS, OUTREACH_BOUNCE_PAUSE_RATE,
// OUTREACH_COMPLAINT_PAUSE_RATE, OUTREACH_SOFT_BOUNCE_MAX).
type Thresholds struct {
	MinSends      int
	BounceRate    float64
	ComplaintRate float64
	SoftBounceMax int
}

// ThresholdsFrom builds Thresholds from the loaded outreach settings.
func ThresholdsFrom(o config.OutreachSettings) Thresholds {
	return Thresholds{MinSends: o.CircuitMinSends, BounceRate: o.BouncePauseRate, ComplaintRate: o.ComplaintPauseRate, SoftBounceMax: o.SoftBounceMax}
}

// RecordBounceAndMaybePause bumps the sequence's bounce (or complaint) counter,
// then auto-pauses it when sent+bounced sends reach MinSends and either rate is
// exceeded (strictly). Reports true only when this call tripped the breaker; an
// empty or unknown sequence, an already auto-paused one, or too few sends is false.
func RecordBounceAndMaybePause(ctx context.Context, tx pgx.Tx, wsID, seqID string, complaint bool, th Thresholds) (bool, error) {
	if seqID == "" {
		return false, nil
	}
	q := gen.New(tx)
	c, err := q.OutreachSeqBumpCounter(ctx, gen.OutreachSeqBumpCounterParams{Complaint: complaint, WorkspaceID: wsID, ID: seqID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("outreachstore: bump counter: %w", err)
	}
	sent, err := q.OutreachSendCountSentOrBounced(ctx, gen.OutreachSendCountSentOrBouncedParams{WorkspaceID: wsID, SequenceID: seqID})
	if err != nil {
		return false, fmt.Errorf("outreachstore: count sends: %w", err)
	}
	if c.AutoPaused || int(sent) < th.MinSends {
		return false, nil
	}
	var bounceRate, complaintRate float64
	if sent != 0 {
		bounceRate = float64(c.BounceCount) / float64(sent)
		complaintRate = float64(c.ComplaintCount) / float64(sent)
	}
	if bounceRate > th.BounceRate || complaintRate > th.ComplaintRate {
		if err := q.OutreachSeqAutoPause(ctx, gen.OutreachSeqAutoPauseParams{WorkspaceID: wsID, ID: seqID}); err != nil {
			return false, fmt.Errorf("outreachstore: auto pause: %w", err)
		}
		return true, nil
	}
	return false, nil
}

// BounceApply is apply_bounce's keyword arguments. SendID and EnrollmentID are nil
// for None; an empty SequenceID is None.
type BounceApply struct {
	WorkspaceID  string
	SendID       *int32
	SequenceID   string
	EnrollmentID *int
	ToEmail      string
	Kind         string // hard (default for any other value) | soft | complaint
	Diagnostic   string
	Source       string
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ApplyBounce is the single bounce/complaint applier. The caller's ledger insert
// is the idempotency gate (the breaker counter is not idempotent): call it only
// when RecordInboundProcessed reported a new row.
func ApplyBounce(ctx context.Context, tx pgx.Tx, a BounceApply, th Thresholds) error {
	src := orDefault(a.Source, "inbound")
	markSend := func(def string) error {
		if a.SendID == nil {
			return nil
		}
		_, err := MarkSendBounced(ctx, tx, a.WorkspaceID, *a.SendID, orDefault(a.Diagnostic, def))
		return err
	}
	advance := func(reason string) error {
		if a.EnrollmentID == nil {
			return nil
		}
		return AdvanceEnrollment(ctx, tx, *a.EnrollmentID, Advance{Status: "bounced", Error: reason})
	}
	switch a.Kind {
	case "complaint":
		if err := markSend("complaint"); err != nil {
			return err
		}
		if _, err := AddSuppression(ctx, tx, a.WorkspaceID, a.ToEmail, "complaint", src, true); err != nil {
			return err
		}
		if err := advance("complaint"); err != nil {
			return err
		}
		_, err := RecordBounceAndMaybePause(ctx, tx, a.WorkspaceID, a.SequenceID, true, th)
		return err
	case "soft":
		if a.EnrollmentID == nil {
			return nil
		}
		n, err := IncrementSoftBounce(ctx, tx, *a.EnrollmentID)
		if err != nil {
			return err
		}
		if n >= th.SoftBounceMax {
			if _, err := AddSuppression(ctx, tx, a.WorkspaceID, a.ToEmail, "bounce", src, false); err != nil {
				return err
			}
			if err := markSend("soft_bounce_max"); err != nil {
				return err
			}
			return advance("soft_bounce_max")
		}
		return nil
	default: // hard
		if err := markSend("hard_bounce"); err != nil {
			return err
		}
		if _, err := AddSuppression(ctx, tx, a.WorkspaceID, a.ToEmail, "bounce", src, false); err != nil {
			return err
		}
		if err := advance("hard_bounce"); err != nil {
			return err
		}
		_, err := RecordBounceAndMaybePause(ctx, tx, a.WorkspaceID, a.SequenceID, false, th)
		return err
	}
}
