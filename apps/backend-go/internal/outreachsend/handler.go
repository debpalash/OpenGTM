package outreachsend

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachtoken"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/smtpclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/smtpmail"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// DefaultJobTimeout is the job cap passed to the SMTP exchange.
const DefaultJobTimeout = 300 * time.Second

// Handler is the `send` job handler (sending._do_send). Sends are not
// idempotent, so the in_flight ledger row is committed before the SMTP dial and
// every step is its own tenancy.InWorkspace transaction; none spans the dial.
type Handler struct {
	Pool    *pgxpool.Pool
	Secrets *secrets.Resolver
	Signer  *outreachtoken.Signer // unsubscribe tokens (master key derived)
	Mailer  Mailer
	// Caps meters paid sends; nil is valid only when SendCostUSD is 0 (D7).
	Caps           Caps
	Automations    bool    // AUTOMATIONS_ENABLED master switch
	BillingEnabled bool    // BILLING_ENABLED
	SendCostUSD    float64 // OUTREACH_SEND_COST_USD
	Thresholds     outreachstore.Thresholds
	Now            func() time.Time
	JobTimeout     time.Duration
	Log            *slog.Logger
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// job is the decoded payload: SendPayload for the typed fields plus presence of
// the nullable ids (SendPayload cannot tell null from 0).
type job struct {
	SendPayload
	hasEnrollment bool
	hasLead       bool
}

func parseJob(raw []byte) (*job, error) {
	j := &job{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // int 5 renders "5", never "5.0"
	if err := dec.Decode(&j.SendPayload); err != nil {
		return nil, fmt.Errorf("outreachsend: bad payload: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("outreachsend: bad payload: %w", err)
	}
	has := func(k string) bool { v, ok := m[k]; return ok && string(bytes.TrimSpace(v)) != "null" }
	j.hasEnrollment, j.hasLead = has("enrollment_id"), has("lead_id")
	return j, nil
}

type step struct {
	number  int
	subject string
	body    string
	delay   int
}

func pyInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		f, _ := x.Float64()
		return int(f)
	case string:
		i, _ := strconv.Atoi(x)
		return i
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func parseSteps(raw json.RawMessage) []step {
	var list []map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &list) != nil {
		return nil
	}
	out := make([]step, 0, len(list))
	for _, s := range list {
		st := step{number: pyInt(s["step_number"]), delay: pyInt(s["delay_hours"])}
		st.subject, _ = s["subject"].(string)
		st.body, _ = s["body_html"].(string)
		out = append(out, st)
	}
	return out
}

func pickStep(steps []step, n int) *step {
	for i := range steps {
		if steps[i].number == n {
			return &steps[i]
		}
	}
	if len(steps) > 0 {
		return &steps[0]
	}
	return nil
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (h *Handler) inWS(ctx context.Context, ws string, fn func(tx pgx.Tx) error) error {
	return tenancy.InWorkspace(ctx, h.Pool, ws, fn)
}

// replayFinalize marks a non-terminal row skipped/replay. The status guard keeps
// a racing replay from clobbering a row another claimer just finalized.
func replayFinalize(ctx context.Context, tx pgx.Tx, ws, idem string) error {
	_, err := tx.Exec(ctx, `UPDATE outreach_sends SET status='skipped', skip_reason='replay', updated_at=now()
	  WHERE workspace_id=$1 AND idempotency_key=$2 AND status NOT IN ('sent','failed','skipped','bounced')`, ws, idem)
	return err
}

func isTerminal(st string) bool {
	return st == "sent" || st == "failed" || st == "skipped" || st == "bounced"
}

// Handle runs one send job.
func (h *Handler) Handle(ctx context.Context, c *queue.Claim) error {
	if !h.Automations {
		return nil // master switch off
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	j, err := parseJob(c.Payload)
	if err != nil {
		return &queue.PermanentError{Err: err}
	}
	ws := j.WorkspaceID
	if ws == "" {
		ws = c.WorkspaceID
	}
	if ws == "" || j.IdempotencyKey == "" {
		return &queue.PermanentError{Err: errors.New("outreachsend: payload needs workspace_id and idempotency_key")}
	}
	if c.WorkspaceID != "" && c.WorkspaceID != ws {
		return &queue.PermanentError{Err: errors.New("outreachsend: payload workspace does not match the job's workspace")}
	}
	cost := h.SendCostUSD
	paid := cost > 0
	if paid && h.Caps == nil {
		return h.handBack(ctx, c, "paid outreach send needs caps (plan 08)")
	}
	timeout := h.JobTimeout
	if timeout <= 0 {
		timeout = DefaultJobTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return h.run(ctx, ws, j, cost, paid)
}

func (h *Handler) handBack(ctx context.Context, c *queue.Claim, why string) error {
	ok, err := queue.HandBack(ctx, h.Pool, c, why)
	if err != nil {
		return err
	}
	if !ok {
		return nil // lease lost: nothing to write
	}
	return fmt.Errorf("%w: %s", workbook.ErrHandedBack, why)
}

type gateOutcome int

const (
	gateGo gateOutcome = iota
	gateStop
)

func (h *Handler) run(ctx context.Context, ws string, j *job, cost float64, paid bool) error {
	idem := j.IdempotencyKey
	seqID := j.SequenceID
	var enrollmentID *int
	if j.hasEnrollment {
		v := j.EnrollmentID
		enrollmentID = &v
	}
	var leadID *int32
	if j.hasLead {
		v := int32(j.LeadID)
		leadID = &v
	}
	in := outreachstore.SendIn{
		WorkspaceID: ws, SequenceID: seqID, StepNumber: int32(j.StepNumber), Subject: j.Subject,
		IdempotencyKey: idem, LeadID: leadID,
	}
	if enrollmentID != nil {
		v := int32(*enrollmentID)
		in.EnrollmentID = &v
	}

	// 1+2. replay / dedup on (workspace, idempotency key).
	var stop bool
	if err := h.inWS(ctx, ws, func(tx pgx.Tx) error {
		ex, err := outreachstore.GetSendByIdem(ctx, tx, ws, idem)
		if err != nil || ex == nil {
			return err
		}
		stop = true
		if isTerminal(ex.Status) {
			return nil
		}
		return replayFinalize(ctx, tx, ws, idem)
	}); err != nil || stop {
		return err
	}

	// 3. load enrollment + sequence; terminal skips.
	var seq *outreachstore.Seq
	to := emailnorm.Normalize(j.ToEmail)
	if err := h.inWS(ctx, ws, func(tx pgx.Tx) error {
		if seqID != "" {
			s, err := outreachstore.GetSequence(ctx, tx, ws, seqID)
			if err != nil {
				return err
			}
			seq = s
		}
		if enrollmentID != nil {
			enr, err := outreachstore.GetEnrollment(ctx, tx, *enrollmentID)
			if err != nil {
				return err
			}
			if enr == nil {
				in.ToEmail = to
				stop = true
				return outreachstore.InsertTerminal(ctx, tx, in, "skipped", "lead_not_found", "")
			}
			to = emailnorm.Normalize(enr.ToEmailSnapshot)
			if seqID == "" {
				seqID = enr.SequenceID
				in.SequenceID = seqID
				if seq, err = outreachstore.GetSequence(ctx, tx, ws, seqID); err != nil {
					return err
				}
			}
		}
		if to == "" {
			stop = true
			return outreachstore.InsertTerminal(ctx, tx, in, "skipped", "no_email", "")
		}
		return nil
	}); err != nil || stop {
		return err
	}
	in.ToEmail = to

	// render the step (variables default to {"email": to}, a Python quirk kept).
	subject, bodyHTML := j.Subject, j.BodyHTML
	var steps []step
	if seq != nil {
		steps = parseSteps(seq.Steps)
		if st := pickStep(steps, j.StepNumber); st != nil {
			vars := j.Variables
			if len(vars) == 0 {
				vars = map[string]any{"email": to}
			}
			subject = smtpmail.Render(st.subject, vars)
			bodyHTML = smtpmail.Render(st.body, vars)
		}
	}

	cfg := outreachcfg.LoadSMTP(ctx, h.Secrets, ws)

	// 4-6. window, suppression, SMTP configured, hourly and daily caps.
	if err := h.inWS(ctx, ws, func(tx pgx.Tx) error {
		resched := func(at time.Time) error {
			if enrollmentID == nil {
				return nil
			}
			return outreachstore.RescheduleEnrollment(ctx, tx, *enrollmentID, at)
		}
		stop = true
		if seq != nil && !InSendWindow(*seq, h.now()) {
			return resched(NextWindowSlot(*seq, h.now()))
		}
		sup, err := outreachstore.IsSuppressed(ctx, tx, ws, to)
		if err != nil {
			return err
		}
		if sup {
			if err := outreachstore.InsertTerminal(ctx, tx, in, "skipped", "suppressed", ""); err != nil {
				return err
			}
			if enrollmentID != nil {
				return outreachstore.AdvanceEnrollment(ctx, tx, *enrollmentID, outreachstore.Advance{Status: "suppressed"})
			}
			return nil
		}
		if !cfg.Configured() {
			return outreachstore.InsertTerminal(ctx, tx, in, "failed", "", "SMTP not configured")
		}
		n, err := outreachstore.SentCountLastHourAt(ctx, tx, ws, h.now())
		if err != nil {
			return err
		}
		if n >= cfg.MaxPerHour {
			return resched(h.now().Add(15 * time.Minute))
		}
		if seq != nil {
			n, err := outreachstore.SentCountTodayAt(ctx, tx, ws, seqID, h.now())
			if err != nil {
				return err
			}
			limit := 50
			if seq.DailyLimit != nil && *seq.DailyLimit != 0 {
				limit = *seq.DailyLimit
			}
			if n >= limit {
				return resched(h.now().Add(time.Hour))
			}
		}
		stop = false
		return nil
	}); err != nil || stop {
		return err
	}

	// 7. caps reservation (the debit waits for a successful handoff).
	var resv Reservation
	if paid {
		trigger := seqID
		if trigger == "" {
			trigger = "outreach"
		}
		if err := h.inWS(ctx, ws, func(tx pgx.Tx) error {
			r, err := h.Caps.TryReserve(ctx, tx, ws, trigger, cost, idem)
			if err != nil {
				return err
			}
			resv = r
			if !r.OK {
				stop = true
				return outreachstore.InsertTerminal(ctx, tx, in, "skipped", "cap", "")
			}
			return nil
		}); err != nil || stop {
			return err
		}
	}

	fctx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	}
	release := func() {
		if !resv.OK {
			return
		}
		c, cancel := fctx()
		defer cancel()
		if err := h.inWS(c, ws, func(tx pgx.Tx) error { return h.Caps.Release(c, tx, resv) }); err != nil {
			h.log().Error("outreach send: release reservation failed", "workspace", ws, "err", err)
		}
	}

	// 8. in_flight, committed before the dial. The context is checked first so a
	// cancelled job writes nothing and sends nothing.
	if err := ctx.Err(); err != nil {
		release()
		return err
	}
	in.Subject = subject
	var created bool
	if err := h.inWS(ctx, ws, func(tx pgx.Tx) error {
		var err error
		created, err = outreachstore.InsertInFlight(ctx, tx, in)
		return err
	}); err != nil {
		release()
		return err
	}
	if !created {
		release()
		c, cancel := fctx()
		defer cancel()
		return h.inWS(c, ws, func(tx pgx.Tx) error { return replayFinalize(c, tx, ws, idem) })
	}

	// 9. build the message (footer and List-Unsubscribe in both parts).
	footer := outreachcfg.Footer(ctx, h.Secrets, ws)
	var htmlBody, textBody string
	var headers map[string]string
	var buildErr error
	if footer == "" {
		buildErr = smtpmail.ErrNoFooter
	} else if h.Signer == nil {
		buildErr = errors.New("unsubscribe signer not configured")
	} else if unsub, err := h.Signer.URL(ws, to, seqID); err != nil {
		buildErr = err
	} else {
		htmlBody, textBody, headers, buildErr = smtpmail.Build(footer, unsub, bodyHTML)
	}
	if buildErr != nil {
		release()
		c, cancel := fctx()
		defer cancel()
		if errors.Is(buildErr, smtpmail.ErrNoFooter) {
			return h.inWS(c, ws, func(tx pgx.Tx) error {
				return outreachstore.FinalizeSend(c, tx, ws, idem, outreachstore.Finalize{Status: "skipped", SkipReason: "no_footer"})
			})
		}
		return h.failTransient(c, ws, idem, enrollmentID, "message build failed: "+buildErr.Error(), nil)
	}

	// 10. SMTP. A cancelled context after the marker is a failed send, not a retry
	// candidate (the marker would make a retry a replay-skip anyway).
	var messageID, errText string
	var hard bool
	switch {
	case ctx.Err() != nil:
		errText = "cancelled before send"
	case !smtpmail.ValidRecipient(to):
		errText = "invalid recipient address"
	default:
		raw, composedID := smtpmail.Compose(smtpmail.Message{
			FromEmail: cfg.Email, FromName: cfg.FromName, To: to, Subject: subject,
			HTML: htmlBody, Text: textBody, Headers: headers,
		})
		id, err := h.Mailer.Send(ctx, MailConfig{Host: cfg.Host, Port: cfg.Port, Email: cfg.Email, Password: string(cfg.Password), UseTLS: cfg.UseTLS}, cfg.Email, to, raw)
		if err != nil {
			errText, hard = classifySendError(err)
		} else if messageID = id; messageID == "" {
			messageID = composedID
		}
	}

	// 11. classify, debit-on-success, settle, finalize. These must land even if
	// the job context expired during the exchange.
	c, cancelF := fctx()
	defer cancelF()
	if errText == "" {
		return h.succeed(c, ws, idem, enrollmentID, seq, steps, j.StepNumber, messageID, cost, paid, resv)
	}
	if hard {
		return h.hardBounce(c, ws, idem, to, seqID, enrollmentID, errText, resv)
	}
	return h.failTransient(c, ws, idem, enrollmentID, errText, &resv)
}

func (h *Handler) succeed(ctx context.Context, ws, idem string, enrollmentID *int, seq *outreachstore.Seq, steps []step, stepNumber int, messageID string, cost float64, paid bool, resv Reservation) error {
	now := h.now()
	advance := func(tx pgx.Tx) error {
		if enrollmentID == nil {
			return nil
		}
		next := stepNumber + 1
		var nd *step
		for i := range steps {
			if steps[i].number == next {
				nd = &steps[i]
				break
			}
		}
		if nd == nil {
			cur := stepNumber
			return outreachstore.AdvanceEnrollment(ctx, tx, *enrollmentID, outreachstore.Advance{Status: "completed", IncrementSent: true, CurrentStep: &cur})
		}
		at := now.Add(time.Duration(nd.delay) * time.Hour)
		return outreachstore.AdvanceEnrollment(ctx, tx, *enrollmentID, outreachstore.Advance{Status: "scheduled", IncrementSent: true, CurrentStep: &next, NextSendAt: &at})
	}
	finish := func(tx pgx.Tx, charged float64) error {
		if resv.OK {
			if err := h.Caps.Settle(ctx, tx, resv); err != nil {
				return err
			}
		}
		if err := outreachstore.FinalizeSend(ctx, tx, ws, idem, outreachstore.Finalize{Status: "sent", MessageID: messageID, SentAt: &now, ChargedUSD: &charged}); err != nil {
			return err
		}
		return advance(tx)
	}
	err := h.inWS(ctx, ws, func(tx pgx.Tx) error {
		charged := 0.0
		if paid {
			res, err := billing.CheckAndDebit(ctx, tx, h.BillingEnabled, ws, cost, idem, "outreach_send")
			if err != nil {
				return err
			}
			if !res.IdempotentReplay {
				charged = res.ChargedUSD
			}
		}
		return finish(tx, charged)
	})
	if err == nil {
		return nil
	}
	// The mail is out: record it as sent (uncharged) rather than leave in_flight.
	h.log().Error("outreach send: debit/finalize failed after SMTP success", "workspace", ws, "err", err)
	if err2 := h.inWS(ctx, ws, func(tx pgx.Tx) error { return finish(tx, 0) }); err2 != nil {
		return fmt.Errorf("outreach send: finalize after success: %w", err2)
	}
	return fmt.Errorf("outreach send: debit failed after successful send: %w", err)
}

func (h *Handler) hardBounce(ctx context.Context, ws, idem, to, seqID string, enrollmentID *int, errText string, resv Reservation) error {
	now := h.now()
	return h.inWS(ctx, ws, func(tx pgx.Tx) error {
		if resv.OK {
			if err := h.Caps.Release(ctx, tx, resv); err != nil {
				return err
			}
		}
		if err := outreachstore.FinalizeSend(ctx, tx, ws, idem, outreachstore.Finalize{Status: "bounced", Error: errText, BouncedAt: &now}); err != nil {
			return err
		}
		src := seqID
		if src == "" {
			src = "send"
		}
		if _, err := outreachstore.AddSuppression(ctx, tx, ws, to, "bounce", src, false); err != nil {
			return err
		}
		if enrollmentID != nil {
			if err := outreachstore.AdvanceEnrollment(ctx, tx, *enrollmentID, outreachstore.Advance{Status: "bounced", Error: errText}); err != nil {
				return err
			}
		}
		_, err := outreachstore.RecordBounceAndMaybePause(ctx, tx, ws, seqID, false, h.Thresholds)
		return err
	})
}

// failTransient finalizes failed (never charged), fails the enrollment, and
// returns the error that makes the queue retry; the retry short-circuits to a
// replay-skip on the terminal row.
func (h *Handler) failTransient(ctx context.Context, ws, idem string, enrollmentID *int, errText string, resv *Reservation) error {
	err := h.inWS(ctx, ws, func(tx pgx.Tx) error {
		if resv != nil && resv.OK {
			if err := h.Caps.Release(ctx, tx, *resv); err != nil {
				return err
			}
		}
		if err := outreachstore.FinalizeSend(ctx, tx, ws, idem, outreachstore.Finalize{Status: "failed", Error: errText}); err != nil {
			return err
		}
		if enrollmentID != nil {
			return outreachstore.AdvanceEnrollment(ctx, tx, *enrollmentID, outreachstore.Advance{Status: "failed", Error: errText})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("outreach send: record failure: %w", err)
	}
	return fmt.Errorf("outreach send transient failure: %s", trunc(errText, 160))
}

// classifySendError returns the text to store and whether the send hard-bounced.
// Only SMTP protocol replies can bounce: they are classified on the shape Python's
// _is_hard_bounce saw, smtplib's "(code, b'msg')", because a bare "535 ..." (AUTH
// failure, our credentials) or "552 ..." must not read as a recipient bounce and
// suppress the address (Python parity). Transport failures (refused, timeout, DNS,
// TLS) are never hard: their Go text carries address and port ("dial tcp
// 52.1.2.3:587: ...", a 5xx-looking token), so a Python-like address-free text is
// stored instead and the send fails for the queue retry.
func classifySendError(err error) (text string, hard bool) {
	var te *textproto.Error
	if errors.As(err, &te) {
		verbatim := fmt.Sprintf("%d %s", te.Code, te.Msg)
		return verbatim, smtpclient.IsHardBounce(fmt.Sprintf("(%d, b'%s')", te.Code, te.Msg))
	}
	return transportText(err), false
}

func transportText(err error) string {
	var (
		dns   *net.DNSError
		cert  *tls.CertificateVerificationError
		alert tls.AlertError
		rhe   tls.RecordHeaderError
		ua    x509.UnknownAuthorityError
		hn    x509.HostnameError
		ci    x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "[Errno 111] Connection refused"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), isTimeout(err):
		return "timed out"
	case errors.As(err, &dns):
		return "name resolution failed"
	case errors.As(err, &cert), errors.As(err, &alert), errors.As(err, &rhe), errors.As(err, &ua), errors.As(err, &hn), errors.As(err, &ci):
		return "TLS handshake failed"
	}
	// Local, address-free failures from smtpclient ("STARTTLS extension not
	// supported by server.", "smtp: plaintext authentication refused ...") keep
	// their text; anything carrying digits (addresses, ports, codes) does not.
	if msg := err.Error(); !strings.ContainsAny(msg, "0123456789") && len(msg) <= 200 {
		return msg
	}
	return "SMTP connection failed"
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
