package autoactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// The legacy outreach actions (D5, behind GO_EXECUTOR_LEGACY_OUTREACH_ACTIONS).
// Neither action sends mail. sequencer enrols the lead; send_email only enqueues
// a `send` job, so SMTP I/O, the suppression re-check, the consent footer, the
// SMTP credential rules, HTML escaping and the at-most-once in_flight marker all
// stay in the existing outreachsend handler. This file never dials SMTP.

func truncErr(err error) string { return truncRunes(err.Error(), 200) }

// leadEmail is normalize_email(lead_data.get("email", "")).
func leadEmail(lead map[string]any) string {
	v, ok := lead["email"]
	if !ok || v == nil {
		return ""
	}
	return emailnorm.Normalize(rowtmpl.PyStr(v))
}

// legacySequencer is _act_sequencer: enrol the row's lead in a sequence of this
// workspace (idempotent). A suppressed address is skipped before any write.
func legacySequencer(ctx context.Context, d Deps, ws string, cfg map[string]any, leadID *int64, lead map[string]any) Result {
	seq := strings.TrimSpace(str0(cfg["sequence_id"]))
	if seq == "" {
		r := failed("sequence_id not configured")
		r.SkipReason = "not_connected"
		return r
	}
	email := leadEmail(lead)
	if email == "" {
		return Result{Status: "skipped", SkipReason: "no_email", Summary: "lead has no email"}
	}
	if leadID == nil {
		return failed(errNoLeadID)
	}
	if d.Pool == nil {
		return failed("outreach store not configured")
	}
	var res Result
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		ok, err := outreachstore.SequenceExists(ctx, tx, ws, seq)
		if err != nil {
			return err
		}
		if !ok {
			res = failed("sequence not found in workspace")
			res.SkipReason = "not_connected"
			return nil
		}
		sup, err := outreachstore.IsSuppressed(ctx, tx, ws, email)
		if err != nil {
			return err
		}
		if sup {
			res = Result{Status: "skipped", SkipReason: "suppressed", Summary: "suppressed"}
			return nil
		}
		_, created, err := outreachstore.Enroll(ctx, tx, ws, seq, int(*leadID), email,
			"trigger:"+str0(cfg["_trigger_id"]), now(d))
		if err != nil {
			return err
		}
		if !created {
			if _, stored, found, err := existingEnrollment(ctx, tx, ws, seq, *leadID); err != nil {
				return err
			} else if found && stored != email {
				res = failed(errEnrollmentEmail)
				return nil
			}
		}
		res = Result{Status: "success", Summary: "already enrolled"}
		if created {
			res.Summary = "enrolled"
		}
		return nil
	})
	if err != nil {
		return failed(truncErr(err))
	}
	return res
}

// sendJob is the `send` job payload with Python's key order; the enrollment and
// lead ids are null (not 0) when unknown, which the send handler tells apart.
type sendJob struct {
	WorkspaceID    string         `json:"workspace_id"`
	SequenceID     *string        `json:"sequence_id"`
	EnrollmentID   *int           `json:"enrollment_id"`
	LeadID         *int64         `json:"lead_id"`
	StepNumber     int            `json:"step_number"`
	ToEmail        string         `json:"to_email"`
	Subject        any            `json:"subject"`
	BodyHTML       any            `json:"body_html"`
	Variables      map[string]any `json:"variables"`
	IdempotencyKey string         `json:"idempotency_key"`
}

// legacySendEmail is _act_send_email: enqueue a `send` job (never sends). The
// per-step key (seq:...) makes the ticker and this action collapse to one send.
// Unlike Python, a suppressed address is skipped here instead of being enrolled
// and queued for the handler to refuse (the handler still re-checks).
func legacySendEmail(ctx context.Context, d Deps, ws string, cfg map[string]any, leadID *int64, lead map[string]any, idem string) Result {
	email := leadEmail(lead)
	if email == "" {
		return Result{Status: "skipped", SkipReason: "no_email", Summary: "lead has no email"}
	}
	var seqID *string
	if s := strings.TrimSpace(str0(cfg["sequence_id"])); s != "" {
		seqID = &s
	}
	step, err := pyIntCfg(cfg["step_number"])
	if err != nil {
		return failed(truncErr(err))
	}
	if seqID != nil && leadID == nil {
		return failed(errNoLeadID)
	}
	if d.Pool == nil {
		return failed("outreach store not configured")
	}
	var res Result
	err = tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		sup, err := outreachstore.IsSuppressed(ctx, tx, ws, email)
		if err != nil {
			return err
		}
		if sup {
			res = Result{Status: "skipped", SkipReason: "suppressed", Summary: "suppressed"}
			return nil
		}
		job := sendJob{WorkspaceID: ws, SequenceID: seqID, LeadID: leadID, StepNumber: step, ToEmail: email,
			Subject: cfgOr(cfg, "subject"), BodyHTML: cfgOr(cfg, "body_html"), Variables: lead}
		key := idem
		if key == "" {
			key = fmt.Sprintf("trig:%s:adhoc:%s:%d", ws, adhocWho(leadID, email), step)
		}
		if seqID != nil {
			ok, err := outreachstore.SequenceExists(ctx, tx, ws, *seqID)
			if err != nil {
				return err
			}
			if !ok {
				res = failed("sequence not found in workspace")
				res.SkipReason = "not_connected"
				return nil
			}
			eid, created, err := outreachstore.Enroll(ctx, tx, ws, *seqID, int(*leadID), email, "trigger_send", time.Time{})
			if err != nil {
				return err
			}
			if !created {
				id, stored, found, err := existingEnrollment(ctx, tx, ws, *seqID, *leadID)
				if err != nil {
					return err
				}
				if found && stored != email {
					res = failed(errEnrollmentEmail)
					return nil
				}
				eid = 0
				if found {
					eid = id
				}
			}
			if eid != 0 {
				job.EnrollmentID = &eid
				key = outreachsend.SequenceStepIdem(ws, *seqID, eid, step)
			}
		}
		job.IdempotencyKey = key
		queued, err := enqueueIfAbsent(ctx, tx, ws, job)
		if err != nil {
			return err
		}
		res = Result{Status: "success", Summary: "already queued/sent"}
		if queued {
			res.Summary = "send enqueued"
		}
		return nil
	})
	if err != nil {
		return failed(truncErr(err))
	}
	return res
}

// Refusals that keep one recipient's data from reaching another (review wave2a M4).
const (
	errNoLeadID        = "trigger row has no lead id; sequence enrollment and sends need a lead"
	errEnrollmentEmail = "existing enrollment for this lead has a different email; not reusing it"
)

// existingEnrollment is the enrollment of (sequence, lead) and its stored (normalized) address.
func existingEnrollment(ctx context.Context, tx pgx.Tx, ws, seq string, leadID int64) (id int, email string, found bool, err error) {
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

// enqueueIfAbsent is _enqueue_if_absent (outreachsend.EnqueueSend with a payload
// that can carry null ids).
func enqueueIfAbsent(ctx context.Context, tx pgx.Tx, ws string, job sendJob) (bool, error) {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE type='send' AND status IN ('pending','processing')
	   AND payload->>'idempotency_key' = $1)`, job.IdempotencyKey).Scan(&pending); err != nil {
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

func now(d Deps) time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

// adhocWho keys a standalone send without an engine key. Python used the literal
// lead id (None for every lead-less row, which collapsed distinct recipients into
// one send); the address stands in when there is no lead id.
func adhocWho(id *int64, email string) string {
	if id != nil {
		return strconv.FormatInt(*id, 10)
	}
	return "e:" + email
}

func str0(v any) string {
	if v == nil {
		return ""
	}
	return rowtmpl.PyStr(v)
}

// cfgOr is cfg.get(k, ""): the value as configured, "" when absent.
func cfgOr(cfg map[string]any, k string) any {
	if v, ok := cfg[k]; ok {
		return v
	}
	return ""
}

// pyIntCfg is int(cfg.get("step_number", 0) or 0).
func pyIntCfg(v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		return int(x), nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i), nil
		}
		f, err := x.Float64()
		return int(f), err
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %q", x)
		}
		i, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %q", x)
		}
		return i, nil
	}
	return 0, fmt.Errorf("int() argument must be a string or a number, not %T", v)
}
