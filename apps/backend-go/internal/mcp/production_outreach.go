package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
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

// Refusals that keep one recipient's data from reaching another (as the
// automations send_email action, internal/autoactions).
const errEnrollmentEmail = "existing enrollment for this lead has a different email; not reusing it"

// mcpSendJob is the `send` job payload (Python key order). The sequence and
// enrollment ids are null, not empty, when the send is ad hoc: the send handler
// tells the two apart.
type mcpSendJob struct {
	WorkspaceID    string         `json:"workspace_id"`
	SequenceID     *string        `json:"sequence_id"`
	EnrollmentID   *int           `json:"enrollment_id"`
	LeadID         int64          `json:"lead_id"`
	StepNumber     int            `json:"step_number"`
	ToEmail        string         `json:"to_email"`
	Subject        string         `json:"subject"`
	BodyHTML       string         `json:"body_html"`
	Variables      map[string]any `json:"variables"`
	IdempotencyKey string         `json:"idempotency_key"`
}

func sendFailed(msg, skip string) SendResult {
	return SendResult{Status: "failed", Error: msg, SkipReason: skip}
}

// QueueSend is actions._act_send_email inside the pipeline transaction: a
// suppressed address is skipped before any write (Python enrolled and queued it
// for the handler to refuse); a sequence send is keyed per (enrollment, step) so
// the sequence ticker and this call collapse to one send; an ad-hoc send uses
// mcp:<ws>:<caller's idempotency key> or trig:<ws>:adhoc:<lead>:<step>. A send already
// pending or recorded under the key is not queued again.
func (p PGOutreachPort) QueueSend(ctx context.Context, tx pgx.Tx, ws string, r QueueSendRequest) (SendResult, error) {
	email, _ := r.LeadData["email"].(string)
	email = emailnorm.Normalize(email)
	if email == "" {
		return SendResult{Status: "skipped", Summary: "lead has no email", SkipReason: "no_email"}, nil
	}
	sup, err := outreachstore.IsSuppressed(ctx, tx, ws, email)
	if err != nil {
		return SendResult{}, err
	}
	if sup {
		return SendResult{Status: "skipped", Summary: "suppressed", SkipReason: "suppressed"}, nil
	}
	job := mcpSendJob{WorkspaceID: ws, LeadID: r.LeadID, StepNumber: r.StepNumber, ToEmail: email,
		Subject: r.Subject, BodyHTML: r.BodyHTML, Variables: r.LeadData}
	// Send keys are global (jobs.fire_key, the pending-job check): a caller's key
	// is namespaced by workspace so another tenant's key can neither block this
	// send nor reveal that it exists (Python used the raw key).
	key := fmt.Sprintf("trig:%s:adhoc:%d:%d", ws, r.LeadID, r.StepNumber)
	if r.IdempotencyKey != "" {
		key = "mcp:" + ws + ":" + r.IdempotencyKey
	}
	if seq := strings.TrimSpace(r.SequenceID); seq != "" {
		job.SequenceID = &seq
		ok, err := outreachstore.SequenceExists(ctx, tx, ws, seq)
		if err != nil {
			return SendResult{}, err
		}
		if !ok {
			return sendFailed("sequence not found in workspace", "not_connected"), nil
		}
		if r.LeadID < 1 || r.LeadID > 1<<31-1 {
			return sendFailed("sequence not found in workspace", "not_connected"), nil // unreachable: no lead, no email
		}
		eid, created, err := outreachstore.Enroll(ctx, tx, ws, seq, int(r.LeadID), email, "trigger_send", time.Time{})
		if err != nil {
			return SendResult{}, err
		}
		if !created {
			id, stored, found, err := enrollmentOf(ctx, tx, ws, seq, r.LeadID)
			if err != nil {
				return SendResult{}, err
			}
			if found && stored != email {
				return sendFailed(errEnrollmentEmail, ""), nil
			}
			eid = 0
			if found {
				eid = id
			}
		}
		if eid != 0 {
			job.EnrollmentID = &eid
			key = outreachsend.SequenceStepIdem(ws, seq, eid, r.StepNumber)
		}
	}
	job.IdempotencyKey = key
	queued, err := enqueueSendOnce(ctx, tx, ws, job)
	if err != nil {
		return SendResult{}, err
	}
	if queued {
		return SendResult{Status: "success", Summary: "send enqueued"}, nil
	}
	return SendResult{Status: "success", Summary: "already queued/sent"}, nil
}

// enrollmentOf is the enrollment of (sequence, lead) and its normalized address.
func enrollmentOf(ctx context.Context, tx pgx.Tx, ws, seq string, leadID int64) (id int, email string, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT id, to_email_snapshot FROM outreach_enrollments
	  WHERE workspace_id=$1 AND sequence_id=$2 AND lead_id=$3`, ws, seq, int32(leadID)).Scan(&id, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return id, emailnorm.Normalize(email), true, nil
}

// enqueueSendOnce is sending._enqueue_if_absent: no second job while one is
// pending or processing under the key, none once a send row holds the key, and
// queue.EnqueueOnce (fire-key lock and unique index) as the backstop. The job
// row rolls back with tx.
func enqueueSendOnce(ctx context.Context, tx pgx.Tx, ws string, job mcpSendJob) (bool, error) {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE type='send' AND status IN ('pending','processing')
	   AND workspace_id = $2 AND payload->>'idempotency_key' = $1)`, job.IdempotencyKey, ws).Scan(&pending); err != nil {
		return false, err
	}
	if pending {
		return false, nil
	}
	existing, err := outreachstore.GetSendByIdem(ctx, tx, ws, job.IdempotencyKey)
	if err != nil {
		return false, err
	}
	if existing != nil {
		return false, nil
	}
	_, created, err := queue.EnqueueOnce(ctx, tx, "send", ws, job, job.IdempotencyKey, "go")
	return created, err
}
