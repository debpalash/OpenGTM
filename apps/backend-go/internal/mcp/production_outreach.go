package mcp

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
)

// PGOutreachPort is the production OutreachPort over the Go outreach store and
// send queue. Every method runs inside the write pipeline's workspace
// transaction, so an enrollment or a queued send commits with the cap
// settlement and the idempotency key, or not at all (Python enqueued in its own
// session, outside the MCP write). Nothing here dials SMTP: send_email only
// enqueues a `send` job, and the outreachsend handler keeps the suppression
// re-check, the SMTP configuration and consent footer rules, HTML escaping of
// lead fields and the at-most-once in_flight marker.
type PGOutreachPort struct {
	Now func() time.Time
}

func (p PGOutreachPort) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (PGOutreachPort) SequenceExists(ctx context.Context, tx pgx.Tx, ws, seqID string) (bool, error) {
	return outreachstore.SequenceExists(ctx, tx, ws, seqID)
}

func (PGOutreachPort) IsSuppressed(ctx context.Context, tx pgx.Tx, ws, email string) (bool, error) {
	return outreachstore.IsSuppressed(ctx, tx, ws, email)
}

// Enroll is store.enroll(seq, lead, email, consent_source, consent_at=now):
// enrolled=false when the (sequence, lead) pair is already enrolled.
func (p PGOutreachPort) Enroll(ctx context.Context, tx pgx.Tx, ws, seqID string, leadID int64, email, consentSource string) (bool, error) {
	if leadID < 1 || leadID > 1<<31-1 {
		return false, nil // not a lead id (the tool resolved no lead for it)
	}
	_, created, err := outreachstore.Enroll(ctx, tx, ws, seqID, int(leadID), email, consentSource, p.now())
	return created, err
}

// QueueSend is actions._act_send_email inside the pipeline transaction, shared
// with the automations send_email action (autoactions.QueueSend): a suppressed
// address is skipped before any write; a sequence send is keyed per (enrollment,
// step) so the sequence ticker and this call collapse to one send; an ad-hoc
// send uses mcp:<ws>:<caller's idempotency key> or trig:<ws>:adhoc:<lead>:<step>.
// A send already pending (in this workspace) or recorded under the key is not
// queued again.
func (PGOutreachPort) QueueSend(ctx context.Context, tx pgx.Tx, ws string, r QueueSendRequest) (SendResult, error) {
	email, _ := r.LeadData["email"].(string)
	email = emailnorm.Normalize(email)
	if email == "" {
		return SendResult{Status: "skipped", Summary: "lead has no email", SkipReason: "no_email"}, nil
	}
	// Send keys are global (jobs.fire_key): a caller's key is namespaced by
	// workspace so another tenant's key can neither block this send nor reveal
	// that it exists (Python used the raw key).
	key := ""
	if r.IdempotencyKey != "" {
		key = "mcp:" + ws + ":" + r.IdempotencyKey
	}
	req := autoactions.SendRequest{Email: email, LeadID: &r.LeadID, StepNumber: r.StepNumber,
		Subject: r.Subject, BodyHTML: r.BodyHTML, Variables: r.LeadData, IdempotencyKey: key}
	if seq := strings.TrimSpace(r.SequenceID); seq != "" {
		req.SequenceID = &seq
	}
	res, err := autoactions.QueueSend(ctx, tx, ws, req)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{Status: res.Status, Summary: res.Summary, SkipReason: res.SkipReason, Error: res.Error}, nil
}
