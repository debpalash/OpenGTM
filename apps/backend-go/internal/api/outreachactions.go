package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Hardening bounds (D3). Python accepts any number of lead ids in one request and holds one session open for all of them.
const (
	enrollMaxIDs      = 5000
	enrollChunk       = 500 // ids per transaction, so a huge list cannot hold locks
	enrollSourceMax   = 120 // outreach_enrollments.consent_source is varchar(120)
	sendsListLimit    = 200
	enrollDefaultSrc  = "manual_enroll"
	startNeedsSMTP    = "SMTP not configured"
	startNeedsConsent = "consent_basis required before activating"
	startNeedsFooter  = "OUTREACH_FOOTER (physical address) required"
)

type actionRoutes struct{ seqRoutes }

// registerOutreachActions registers the 6 sequence action routes (Task 21). Start, pause, enroll and execute need
// outreach.write (admin); stats and sends need any member.
func registerOutreachActions(r *httpx.Router, d Deps) {
	h := actionRoutes{seqRoutes{d: d}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	read := []httpx.Middleware{authn, identity.Workspace(d.Identity)}
	write := []httpx.Middleware{authn, identity.Workspace(d.Identity), identity.RequireRole("outreach.write", "admin")}
	r.Handle("POST /api/outreach/sequences/{seq_id}/start", http.HandlerFunc(h.start), write...)
	r.Handle("POST /api/outreach/sequences/{seq_id}/pause", http.HandlerFunc(h.pause), write...)
	r.Handle("POST /api/outreach/sequences/{seq_id}/enroll", http.HandlerFunc(h.enroll), write...)
	r.Handle("POST /api/outreach/sequences/{seq_id}/execute", http.HandlerFunc(h.execute), write...)
	r.Handle("GET /api/outreach/sequences/{seq_id}/stats", http.HandlerFunc(h.stats), read...)
	r.Handle("GET /api/outreach/sequences/{seq_id}/sends", http.HandlerFunc(h.sends), read...)
}

func (h actionRoutes) executor(ws string) string {
	return outreachsend.ExecutorFor(config.ExecutorCohortFrom(h.d.getenv()), h.d.Flags.Outreach, ws)
}

func (h actionRoutes) tickConfig() outreachsend.TickConfig {
	o := h.d.Flags.Outreach
	return outreachsend.TickConfig{
		Interval: time.Duration(o.TickInterval) * time.Second, MaxEnqueue: o.TickMaxEnqueue,
		Executors: config.ExecutorCohortFrom(h.d.getenv()), Outreach: o,
	}
}

func (h actionRoutes) start(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	ctx, id := r.Context(), r.PathValue("seq_id")
	var seq *outreachstore.Seq
	err := tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		seq, e = outreachstore.GetSequence(ctx, tx, ws, id)
		return e
	})
	if err != nil {
		h.fail(w, "start", err)
		return
	}
	if seq == nil {
		seqNotFound(w)
		return
	}
	if h.d.Envelope == nil { // no master key: stored SMTP secrets cannot be decrypted
		httpx.Detail(w, http.StatusServiceUnavailable, "Secrets are not configured")
		return
	}
	res := secrets.NewResolver(h.d.Envelope, secrets.NewPgStore(h.d.Pool), h.d.getenv())
	if !outreachcfg.LoadSMTP(ctx, res, ws).Configured() {
		httpx.Detail(w, http.StatusBadRequest, startNeedsSMTP)
		return
	}
	if strings.TrimSpace(seq.ConsentBasis) == "" {
		httpx.Detail(w, http.StatusBadRequest, startNeedsConsent)
		return
	}
	if outreachcfg.Footer(ctx, res, ws) == "" {
		httpx.Detail(w, http.StatusBadRequest, startNeedsFooter)
		return
	}
	var set *outreachstore.Seq
	err = tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		set, e = outreachstore.SetSequenceStatus(ctx, tx, ws, id, "active")
		return e
	})
	if err != nil {
		h.fail(w, "start", err)
		return
	}
	if set == nil { // deleted between the read and the write
		seqNotFound(w)
		return
	}
	// The initial tick runs after the status commit on a context that outlives a client disconnect; its failure
	// is logged and swallowed (the ticker retries), as in Python.
	if _, err := outreachsend.Tick(context.WithoutCancel(ctx), h.d.Pool, ws, id, time.Now(), h.tickConfig()); err != nil {
		h.d.logger().Warn("outreach: initial tick failed", "err_type", errTypeName(err))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

func (h actionRoutes) pause(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	var found bool
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		seq, err := outreachstore.SetSequenceStatus(r.Context(), tx, ws, r.PathValue("seq_id"), "paused")
		if err != nil || seq == nil {
			return err
		}
		found = true
		return outreachstore.DisableSchedule(r.Context(), tx, seq.ID)
	})
	if err != nil {
		h.fail(w, "pause", err)
		return
	}
	if !found {
		seqNotFound(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

// enrollRequest is EnrollLeadsRequest.
type enrollRequest struct {
	LeadIDs       []int64
	ConsentSource string
}

func parseEnroll(raw []byte) (enrollRequest, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return enrollRequest{}, errs
	}
	var req enrollRequest
	loc := seqLoc("lead_ids")
	v, present := obj.Get("lead_ids")
	switch {
	case !present:
		errs = append(errs, seqFE("missing", "Field required", loc, obj, nil))
	default:
		l, isList := v.([]any)
		switch {
		case !isList:
			errs = append(errs, seqFE("list_type", "Input should be a valid list", loc, v, nil))
		case len(l) > enrollMaxIDs:
			errs = append(errs, seqFE("too_long", fmt.Sprintf("List should have at most %d items after validation, not %d", enrollMaxIDs, len(l)),
				loc, "<omitted>", map[string]any{"field_type": "List", "max_length": enrollMaxIDs, "actual_length": len(l)}))
		default:
			req.LeadIDs = make([]int64, 0, len(l))
			for i, item := range l {
				n, e := watchInt(item, append(append([]any{}, loc...), i))
				if e != nil {
					errs = append(errs, *e)
					continue
				}
				req.LeadIDs = append(req.LeadIDs, n)
			}
		}
	}
	if v, present := obj.Get("consent_source"); present {
		s, e := seqString(v, seqLoc("consent_source"), enrollSourceMax)
		if e != nil {
			errs = append(errs, *e)
		}
		req.ConsentSource = s
	}
	if errs != nil {
		return enrollRequest{}, errs
	}
	return req, nil
}

type enrollSkip struct {
	LeadID int64  `json:"lead_id"`
	Reason string `json:"reason"`
}

func (h actionRoutes) enroll(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	req, errs := parseEnroll(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	source := req.ConsentSource
	if source == "" {
		source = enrollDefaultSrc
	}
	ctx, id := r.Context(), r.PathValue("seq_id")
	now := time.Now().UTC()
	enrolled, skipped := 0, []enrollSkip{}
	// The sequence is checked first, even for an empty list (Python order). Every chunk is its own transaction and
	// commits before the next one starts, so a failure part-way keeps the earlier chunks (idempotent to retry:
	// they answer already_enrolled).
	first := true
	for start := 0; first || start < len(req.LeadIDs); start += enrollChunk {
		first = false
		end := min(start+enrollChunk, len(req.LeadIDs))
		chunk := req.LeadIDs[start:end]
		var chunkEnrolled int
		var chunkSkipped []enrollSkip
		exists := true
		err := tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) (e error) {
			chunkEnrolled, chunkSkipped = 0, nil
			if exists, e = outreachstore.SequenceExists(ctx, tx, ws, id); e != nil || !exists {
				return e
			}
			chunkEnrolled, chunkSkipped, e = enrollChunkIn(ctx, tx, ws, id, chunk, source, now)
			return e
		})
		if err != nil {
			h.fail(w, "enroll", err)
			return
		}
		if !exists {
			seqNotFound(w)
			return
		}
		enrolled += chunkEnrolled
		skipped = append(skipped, chunkSkipped...)
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Enrolled     int          `json:"enrolled"`
		Skipped      []enrollSkip `json:"skipped"`
		TotalLeadIDs int          `json:"total_lead_ids"`
	}{enrolled, skipped, len(req.LeadIDs)})
}

func enrollChunkIn(ctx context.Context, tx pgx.Tx, ws, seqID string, ids []int64, source string, now time.Time) (int, []enrollSkip, error) {
	var lookup []int64
	for _, id := range ids {
		if id >= 1 && id <= math.MaxInt32 { // anything else cannot be a lead id (Python answers 500)
			lookup = append(lookup, id)
		}
	}
	emails := map[int64]string{}
	if len(lookup) > 0 {
		leads, err := leadsread.LeadsByIDs(ctx, tx, lookup)
		if err != nil {
			return 0, nil, err
		}
		for _, l := range leads {
			if l.Email != nil {
				emails[l.ID] = emailnorm.Normalize(*l.Email)
			}
		}
	}
	enrolled := 0
	var skipped []enrollSkip
	for _, id := range ids {
		email := emails[id]
		if email == "" {
			skipped = append(skipped, enrollSkip{id, "no_email"})
			continue
		}
		sup, err := outreachstore.IsSuppressed(ctx, tx, ws, email)
		if err != nil {
			return 0, nil, err
		}
		if sup {
			skipped = append(skipped, enrollSkip{id, "suppressed"})
			continue
		}
		_, created, err := outreachstore.Enroll(ctx, tx, ws, seqID, int(id), email, source, now)
		if err != nil {
			return 0, nil, err
		}
		if created {
			enrolled++
		} else {
			skipped = append(skipped, enrollSkip{id, "already_enrolled"})
		}
	}
	return enrolled, skipped, nil
}

func (h actionRoutes) execute(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	ctx, id := r.Context(), r.PathValue("seq_id")
	var n int
	var exists bool
	err := tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		if exists, e = outreachstore.SequenceExists(ctx, tx, ws, id); e != nil || !exists {
			return e
		}
		n, e = outreachsend.EnqueueDue(ctx, tx, ws, id, h.d.Flags.Outreach.TickMaxEnqueue, h.executor(ws))
		return e
	})
	if err != nil {
		h.fail(w, "execute", err)
		return
	}
	if !exists {
		seqNotFound(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"enqueued": n})
}

func (h actionRoutes) stats(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	ctx, id := r.Context(), r.PathValue("seq_id")
	var st map[string]int
	var exists bool
	err := tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		if exists, e = outreachstore.SequenceExists(ctx, tx, ws, id); e != nil || !exists {
			return e
		}
		st, e = outreachstore.SequenceStats(ctx, tx, ws, id)
		return e
	})
	if err != nil {
		h.fail(w, "stats", err)
		return
	}
	if !exists {
		seqNotFound(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, seqStats(st))
}

// sendOut is _send_to_dict.
type sendOut struct {
	ID           int32   `json:"id"`
	SequenceID   string  `json:"sequence_id"`
	EnrollmentID *int32  `json:"enrollment_id"`
	LeadID       *int32  `json:"lead_id"`
	StepNumber   int32   `json:"step_number"`
	ToEmail      string  `json:"to_email"`
	Subject      string  `json:"subject"`
	Status       string  `json:"status"`
	SkipReason   *string `json:"skip_reason"`
	MessageID    string  `json:"message_id"`
	ChargedUSD   pyFloat `json:"charged_usd"`
	Migrated     bool    `json:"migrated"`
	Error        string  `json:"error"`
	SentAt       *string `json:"sent_at"`
	CreatedAt    *string `json:"created_at"`
}

// sendTime is pyISO of an optional time.
func sendTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	return pyISO(pgtype.Timestamptz{Time: *t, Valid: true})
}

func (h actionRoutes) sends(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	// No existence check, as Python: a foreign or unknown id is an empty list.
	var rows []outreachstore.SendRef
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		rows, e = outreachstore.ListSends(r.Context(), tx, ws, r.PathValue("seq_id"), sendsListLimit)
		return e
	})
	if err != nil {
		h.fail(w, "sends", err)
		return
	}
	out := make([]sendOut, 0, len(rows))
	for _, s := range rows {
		o := sendOut{ID: s.ID, SequenceID: s.SequenceID, EnrollmentID: s.EnrollmentID, LeadID: s.LeadID, StepNumber: s.StepNumber,
			ToEmail: s.ToEmail, Subject: s.Subject, Status: s.Status, MessageID: s.MessageID, ChargedUSD: pyFloat(s.ChargedUSD),
			Migrated: s.Migrated, Error: s.Error, SentAt: sendTime(s.SentAt), CreatedAt: sendTime(s.CreatedAt)}
		if s.SkipReason != "" {
			sr := s.SkipReason
			o.SkipReason = &sr
		}
		out = append(out, o)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sends": out})
}
