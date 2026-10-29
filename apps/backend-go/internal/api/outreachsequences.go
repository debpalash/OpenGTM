package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // IANA names resolve without a system zoneinfo

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/jackc/pgx/v5"
)

// Hardening bounds (D3). Python accepts any str/int and lets Postgres answer 500 on varchar overflow or an
// integer past int4; Go answers a pydantic-shaped 422 instead.
const (
	seqNameMax        = 200
	seqDescMax        = 5000
	seqTZMax          = 40
	seqConsentMax     = 40
	seqMaxSteps       = 50
	seqMaxDelayHours  = 8760
	seqMaxDailyLimit  = 100000
	seqStatusLiterals = "'draft', 'active', 'paused' or 'completed'"
)

var seqStatuses = []string{"draft", "active", "paused", "completed"}

func seqFE(typ, msg string, loc []any, in any, ctx map[string]any) httpx.FieldError {
	return httpx.FieldError{Type: typ, Loc: loc, Msg: msg, Input: in, Ctx: ctx}
}

func seqLoc(rest ...any) []any { return append([]any{"body"}, rest...) }

// seqString is `str` with pydantic length bounds (counted in code points). NUL is refused because Postgres
// text cannot store it.
func seqString(v any, loc []any, maxLen int) (string, *httpx.FieldError) {
	s, e := watchStr(v, loc)
	if e != nil {
		return "", e
	}
	if n := len([]rune(s)); n > maxLen {
		fe := seqFE("string_too_long", fmt.Sprintf("String should have at most %d characters", maxLen), loc, v, map[string]any{"max_length": maxLen})
		return "", &fe
	}
	if strings.ContainsRune(s, 0) {
		fe := seqFE("value_error", "Value error, string must not contain NUL characters", loc, v, nil)
		return "", &fe
	}
	return s, nil
}

func seqName(v any, loc []any) (string, *httpx.FieldError) {
	s, e := seqString(v, loc, seqNameMax)
	if e == nil && s == "" {
		fe := seqFE("string_too_short", "String should have at least 1 character", loc, v, map[string]any{"min_length": 1})
		return "", &fe
	}
	return s, e
}

// seqIntRange is `int` (pydantic lax) with ge/le bounds.
func seqIntRange(v any, loc []any, lo, hi int64) (int64, *httpx.FieldError) {
	n, e := watchInt(v, loc)
	if e != nil {
		return 0, e
	}
	if n < lo {
		fe := seqFE("greater_than_equal", "Input should be greater than or equal to "+strconv.FormatInt(lo, 10), loc, v, map[string]any{"ge": lo})
		return 0, &fe
	}
	if n > hi {
		fe := seqFE("less_than_equal", "Input should be less than or equal to "+strconv.FormatInt(hi, 10), loc, v, map[string]any{"le": hi})
		return 0, &fe
	}
	return n, nil
}

func seqTZ(v any, loc []any) (string, *httpx.FieldError) {
	s, e := seqString(v, loc, seqTZMax)
	if e != nil {
		return "", e
	}
	if s == "" || s == "Local" {
		fe := seqFE("value_error", "Value error, send_window_tz must be an IANA timezone name", loc, v, nil)
		return "", &fe
	}
	if _, err := time.LoadLocation(s); err != nil {
		fe := seqFE("value_error", "Value error, send_window_tz must be an IANA timezone name", loc, v, nil)
		return "", &fe
	}
	return s, nil
}

// seqStep is StepInput.model_dump(): this field order is the stored JSON key order.
type seqStep struct {
	StepNumber int64  `json:"step_number"`
	Subject    string `json:"subject"`
	BodyHTML   string `json:"body_html"`
	DelayHours int64  `json:"delay_hours"`
}

// seqSteps is List[StepInput] (at most 50). The result is the JSON text stored verbatim.
func seqSteps(v any, loc []any) (json.RawMessage, []httpx.FieldError) {
	l, ok := v.([]any)
	if !ok {
		return nil, []httpx.FieldError{seqFE("list_type", "Input should be a valid list", loc, v, nil)}
	}
	if len(l) > seqMaxSteps {
		return nil, []httpx.FieldError{seqFE("too_long", fmt.Sprintf("List should have at most %d items after validation, not %d", seqMaxSteps, len(l)),
			loc, "<omitted>", map[string]any{"field_type": "List", "max_length": seqMaxSteps, "actual_length": len(l)})}
	}
	var errs []httpx.FieldError
	steps := make([]seqStep, 0, len(l))
	for i, item := range l {
		at := func(k string) []any { return append(append([]any{}, loc...), i, k) }
		o, isObj := item.(*wbcrud.Obj)
		if !isObj {
			errs = append(errs, seqFE("model_attributes_type", "Input should be a valid dictionary or object to extract fields from", append(append([]any{}, loc...), i), item, nil))
			continue
		}
		var st seqStep
		bad := false
		field := func(k string, parse func(any) *httpx.FieldError) {
			fv, present := o.Get(k)
			if !present {
				errs = append(errs, seqFE("missing", "Field required", at(k), o, nil))
				bad = true
				return
			}
			if e := parse(fv); e != nil {
				errs = append(errs, *e)
				bad = true
			}
		}
		field("step_number", func(fv any) *httpx.FieldError {
			n, e := watchInt(fv, at("step_number"))
			st.StepNumber = n
			return e
		})
		field("subject", func(fv any) *httpx.FieldError {
			s, e := watchStr(fv, at("subject"))
			st.Subject = s
			return e
		})
		field("body_html", func(fv any) *httpx.FieldError {
			s, e := watchStr(fv, at("body_html"))
			st.BodyHTML = s
			return e
		})
		if fv, present := o.Get("delay_hours"); present {
			n, e := seqIntRange(fv, at("delay_hours"), 0, seqMaxDelayHours)
			if e != nil {
				errs = append(errs, *e)
				bad = true
			}
			st.DelayHours = n
		}
		if !bad {
			steps = append(steps, st)
		}
	}
	if errs != nil {
		return nil, errs
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(steps); err != nil {
		return nil, []httpx.FieldError{seqFE("json_invalid", "JSON encode error", loc, nil, nil)}
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// seqFieldOrder is the declaration order of CreateSequenceRequest / UpdateSequenceRequest, which is the order
// pydantic reports errors in.
var seqFieldOrder = []string{"name", "description", "steps", "status", "daily_limit", "send_window_start", "send_window_end", "send_window_tz", "consent_basis"}

// seqField validates one body field. The value type is string, int64 or json.RawMessage (steps).
func seqField(k string, v any) (any, []httpx.FieldError) {
	loc := seqLoc(k)
	one := func(val any, e *httpx.FieldError) (any, []httpx.FieldError) {
		if e != nil {
			return nil, []httpx.FieldError{*e}
		}
		return val, nil
	}
	switch k {
	case "name":
		s, e := seqName(v, loc)
		return one(s, e)
	case "description":
		s, e := seqString(v, loc, seqDescMax)
		return one(s, e)
	case "steps":
		return stepsOrNil(seqSteps(v, loc))
	case "status":
		s, e := watchStr(v, loc)
		if e != nil {
			return nil, []httpx.FieldError{*e}
		}
		if !slices.Contains(seqStatuses, s) {
			return nil, []httpx.FieldError{seqFE("literal_error", "Input should be "+seqStatusLiterals, loc, v, map[string]any{"expected": seqStatusLiterals})}
		}
		if s == "active" {
			return nil, []httpx.FieldError{seqFE("value_error",
				"Value error, status cannot be set to active here: use POST /api/outreach/sequences/{seq_id}/start (it checks SMTP, consent and the footer)", loc, v, nil)}
		}
		return s, nil
	case "daily_limit":
		n, e := seqIntRange(v, loc, 1, seqMaxDailyLimit)
		return one(n, e)
	case "send_window_start", "send_window_end":
		n, e := seqIntRange(v, loc, 0, 23)
		return one(n, e)
	case "send_window_tz":
		s, e := seqTZ(v, loc)
		return one(s, e)
	default: // consent_basis
		s, e := seqString(v, loc, seqConsentMax)
		return one(s, e)
	}
}

func stepsOrNil(raw json.RawMessage, errs []httpx.FieldError) (any, []httpx.FieldError) {
	if errs != nil {
		return nil, errs
	}
	return raw, nil
}

func seqBodyObject(raw []byte) (*wbcrud.Obj, []httpx.FieldError) {
	return wbcrud.DecodeBodyObject(raw)
}

// seqCreate is a validated CreateSequenceRequest.
type seqCreate struct{ In outreachstore.SeqInput }

// parseSeqCreate validates CreateSequenceRequest (name required, every other field defaulted).
func parseSeqCreate(raw []byte) (seqCreate, []httpx.FieldError) {
	obj, errs := seqBodyObject(raw)
	if errs != nil {
		return seqCreate{}, errs
	}
	in := outreachstore.NewSeqInput("")
	in.Steps = json.RawMessage("[]")
	for _, k := range seqFieldOrder {
		v, present := obj.Get(k)
		if !present {
			if k == "name" {
				errs = append(errs, seqFE("missing", "Field required", seqLoc(k), obj, nil))
			}
			continue
		}
		if k == "status" { // not a CreateSequenceRequest field: extras are ignored
			continue
		}
		val, ferrs := seqField(k, v)
		if ferrs != nil {
			errs = append(errs, ferrs...)
			continue
		}
		switch k {
		case "name":
			in.Name = val.(string)
		case "description":
			in.Description = val.(string)
		case "steps":
			in.Steps = val.(json.RawMessage)
		case "daily_limit":
			in.DailyLimit = int(val.(int64))
		case "send_window_start":
			in.SendWindowStart = int(val.(int64))
		case "send_window_end":
			in.SendWindowEnd = int(val.(int64))
		case "send_window_tz":
			in.SendWindowTZ = val.(string)
		case "consent_basis":
			in.ConsentBasis = val.(string)
		}
	}
	if errs != nil {
		return seqCreate{}, errs
	}
	return seqCreate{In: in}, nil
}

// parseSeqUpdate validates UpdateSequenceRequest and returns model_dump(exclude_none=True): only the keys
// present with a non-null value.
func parseSeqUpdate(raw []byte) (map[string]any, []httpx.FieldError) {
	obj, errs := seqBodyObject(raw)
	if errs != nil {
		return nil, errs
	}
	out := map[string]any{}
	for _, k := range seqFieldOrder {
		v, present := obj.Get(k)
		if !present || v == nil {
			continue
		}
		val, ferrs := seqField(k, v)
		if ferrs != nil {
			errs = append(errs, ferrs...)
			continue
		}
		out[k] = val
	}
	if errs != nil {
		return nil, errs
	}
	return out, nil
}

// seqStatsOrder is get_sequence_stats' dict order.
var seqStatsOrder = []string{"pending", "scheduled", "sent", "opened", "replied", "bounced", "failed", "skipped", "suppressed",
	"completed", "total", "emails_sent", "bounce_count", "complaint_count", "auto_paused"}

type seqStats map[string]int

func (s seqStats) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for _, k := range seqStatsOrder {
		v, ok := s[k]
		if !ok {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(strconv.Quote(k))
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(v))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// seqWithStats is the GET body: _seq_to_dict plus "stats".
type seqWithStats struct {
	outreachstore.Seq
	Stats seqStats `json:"stats"`
}

type seqRoutes struct{ d Deps }

// registerOutreachSequences registers the 5 sequence CRUD routes (Task 20). Reads need any member
// (current_workspace); writes need outreach.write (admin).
func registerOutreachSequences(r *httpx.Router, d Deps) {
	h := seqRoutes{d: d}
	authn := identity.Authenticate(d.Signer, d.Identity)
	read := []httpx.Middleware{authn, identity.Workspace(d.Identity)}
	write := []httpx.Middleware{authn, identity.Workspace(d.Identity), identity.RequireRole("outreach.write", "admin")}
	r.Handle("GET /api/outreach/sequences", http.HandlerFunc(h.list), read...)
	r.Handle("POST /api/outreach/sequences", http.HandlerFunc(h.create), write...)
	r.Handle("GET /api/outreach/sequences/{seq_id}", http.HandlerFunc(h.get), read...)
	r.Handle("PUT /api/outreach/sequences/{seq_id}", http.HandlerFunc(h.update), write...)
	r.Handle("DELETE /api/outreach/sequences/{seq_id}", http.HandlerFunc(h.remove), write...)
}

func (h seqRoutes) ws(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return "", false
	}
	return c.WorkspaceID, true
}

func (h seqRoutes) fail(w http.ResponseWriter, what string, err error) {
	h.d.logger().Error("outreach sequences: request failed", "op", what, "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func seqNotFound(w http.ResponseWriter) {
	httpx.Detail(w, http.StatusNotFound, "Sequence not found")
}

func (h seqRoutes) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, ok := wbcrud.ReadBody(r.Body)
	if !ok {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
	}
	return raw, ok
}

func (h seqRoutes) list(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	var seqs []outreachstore.Seq
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		seqs, e = outreachstore.ListSequences(r.Context(), tx, ws)
		return e
	})
	if err != nil {
		h.fail(w, "list", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sequences": seqs})
}

func (h seqRoutes) create(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	c, errs := parseSeqCreate(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var seq outreachstore.Seq
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		seq, e = outreachstore.CreateSequence(r.Context(), tx, ws, c.In)
		return e
	})
	if err != nil {
		h.fail(w, "create", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		ID     string  `json:"id"`
		Name   string  `json:"name"`
		Status *string `json:"status"`
	}{seq.ID, seq.Name, seq.Status})
}

func (h seqRoutes) get(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	id := r.PathValue("seq_id")
	var out *seqWithStats
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		s, err := outreachstore.GetSequence(r.Context(), tx, ws, id)
		if err != nil || s == nil {
			return err
		}
		st, err := outreachstore.SequenceStats(r.Context(), tx, ws, id)
		if err != nil {
			return err
		}
		out = &seqWithStats{Seq: *s, Stats: st}
		return nil
	})
	if err != nil {
		h.fail(w, "get", err)
		return
	}
	if out == nil {
		seqNotFound(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h seqRoutes) update(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	updates, errs := parseSeqUpdate(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var seq *outreachstore.Seq
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		seq, e = outreachstore.UpdateSequence(r.Context(), tx, ws, r.PathValue("seq_id"), updates)
		return e
	})
	if err != nil {
		h.fail(w, "update", err)
		return
	}
	if seq == nil {
		seqNotFound(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}{"ok", seq.ID})
}

func (h seqRoutes) remove(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	var found bool
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		found, e = outreachstore.DeleteSequence(r.Context(), tx, ws, r.PathValue("seq_id"))
		return e
	})
	if err != nil {
		h.fail(w, "delete", err)
		return
	}
	if !found {
		seqNotFound(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
