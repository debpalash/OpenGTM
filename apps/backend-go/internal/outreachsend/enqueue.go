package outreachsend

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// SendPayload is the payload of a `send` job (key order matches Python's
// enqueue_due_sends payload followed by idempotency_key; queue.EnqueueOnce appends
// fire_key). Subject, BodyHTML and Variables are omitted when empty, as Python's
// due-step payload does not carry them.
type SendPayload struct {
	WorkspaceID    string         `json:"workspace_id"`
	SequenceID     string         `json:"sequence_id"`
	EnrollmentID   int            `json:"enrollment_id"`
	LeadID         int            `json:"lead_id"`
	StepNumber     int            `json:"step_number"`
	ToEmail        string         `json:"to_email"`
	IdempotencyKey string         `json:"idempotency_key"`
	Subject        string         `json:"subject,omitempty"`
	BodyHTML       string         `json:"body_html,omitempty"`
	Variables      map[string]any `json:"variables,omitempty"`
}

// ExecutorFor is outreach_executor("send", ws): "go" only when GO_EXECUTOR_ENABLED,
// the workspace is in the cohort and GO_EXECUTOR_OUTREACH_SEND is on.
func ExecutorFor(ex config.ExecutorSettings, o config.OutreachSettings, wsID string) string {
	v, _ := ex.OutreachExecutor(o, "send", wsID) // "send" is always a valid job type
	return v
}

// SequenceStepIdem is sequence_step_idem: seq:<ws>:<seq>:<enrollment>:<step>.
func SequenceStepIdem(wsID, seqID string, enrollmentID, step int) string {
	return fmt.Sprintf("seq:%s:%s:%d:%d", wsID, seqID, enrollmentID, step)
}

// EnqueueSend is _enqueue_if_absent: enqueue a `send` job unless a pending or
// processing send job already carries the idempotency key or a send row exists for
// it. The insert is single-flight per idempotency key (EnqueueOnce fire_key) and
// rolls back with tx, which must be inside tenancy.InWorkspace.
func EnqueueSend(ctx context.Context, tx pgx.Tx, wsID string, p SendPayload, executor string) (bool, error) {
	if p.IdempotencyKey == "" {
		return false, errors.New("outreachsend: EnqueueSend needs an idempotency key")
	}
	var pending bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE type='send' AND status IN ('pending','processing')
		   AND payload->>'idempotency_key' = $1)`, p.IdempotencyKey).Scan(&pending); err != nil {
		return false, fmt.Errorf("outreachsend: pending lookup: %w", err)
	}
	if pending {
		return false, nil
	}
	existing, err := outreachstore.GetSendByIdem(ctx, tx, wsID, p.IdempotencyKey)
	if err != nil {
		return false, fmt.Errorf("outreachsend: send lookup: %w", err)
	}
	if existing != nil {
		return false, nil
	}
	_, created, err := queue.EnqueueOnce(ctx, tx, "send", wsID, p, p.IdempotencyKey, executor)
	return created, err
}

// EnqueueDue is enqueue_due_sends: a send job per due enrollment step (idem from
// current_step), stopping at limit. Returns the number enqueued.
func EnqueueDue(ctx context.Context, tx pgx.Tx, wsID, seqID string, limit int, executor string) (int, error) {
	if executor != "go" && executor != "python" {
		return 0, fmt.Errorf("outreachsend: executor %q must be go or python", executor)
	}
	due, err := outreachstore.DueEnrollments(ctx, tx, seqID, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, enr := range due {
		step := enr.CurrentStep
		ok, err := EnqueueSend(ctx, tx, wsID, SendPayload{
			WorkspaceID: wsID, SequenceID: seqID, EnrollmentID: enr.ID, LeadID: enr.LeadID,
			StepNumber: step, ToEmail: enr.ToEmailSnapshot,
			IdempotencyKey: SequenceStepIdem(wsID, seqID, enr.ID, step),
		}, executor)
		if err != nil {
			return n, err
		}
		if ok {
			n++
			if n >= limit {
				break
			}
		}
	}
	return n, nil
}
