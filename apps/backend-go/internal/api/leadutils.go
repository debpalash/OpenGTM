package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadscore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	// maxScoreBody caps POST /api/leads/score (Python has no cap): an inline lead is one record.
	maxScoreBody = 1 << 20
	// maxUtilBody caps verify-email and tech-stack bodies: one address or one domain.
	maxUtilBody = 64 << 10
)

// Test seam: nil means the production verifier wiring.
var leadUtilsVerifyDeps func(d Deps) emailverify.Deps

type leadUtils struct {
	d     Deps
	vOnce sync.Once
	vd    emailverify.Deps // production cascade wiring, built on first use
}

// registerLeadUtils mounts POST /api/leads/{score,verify-email,tech-stack} (routers/leads.py single-lead utilities).
// All three sit behind current_workspace only: any member, viewers included, may call them.
func registerLeadUtils(r *httpx.Router, d Deps) {
	h := &leadUtils{d: d}
	mw := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	r.Handle("POST /api/leads/score", http.HandlerFunc(h.score), mw...)
	r.Handle("POST /api/leads/verify-email", h.limited("60/minute", maxUtilBody, "email", h.verifyEmail), mw...)
	r.Handle("POST /api/leads/tech-stack", h.limited("30/minute", maxUtilBody, "domain", h.techStack), mw...)
}

// utilCtxKey carries the validated string field from the validating handler to the limited one.
type utilCtxKey struct{}

// limited is FastAPI + slowapi order: current_workspace (the middleware chain), then body validation (a 422 is
// answered before the limiter, so it never counts), then the rate limit, then the route body (a 400 counts).
func (h *leadUtils) limited(spec string, max int64, field string, fn func(http.ResponseWriter, *http.Request, string)) http.Handler {
	var inner http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fn(w, r, r.Context().Value(utilCtxKey{}).(string))
	})
	if h.d.Limiter != nil {
		inner = h.d.Limiter.Limit(spec)(inner)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := readUtilBody(w, r, max)
		if !ok {
			return
		}
		v, errs := parseStringField(raw, field)
		if errs != nil {
			httpx.ValidationError(w, errs...)
			return
		}
		inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), utilCtxKey{}, v)))
	})
}

// readUtilBody reads the request body up to max; an oversized one is answered 413.
func readUtilBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	return raw, true
}

// scoreBody is ScoreLeadBody: lead_id: Optional[int], lead: Optional[dict].
type scoreBody struct {
	LeadID *int64
	Lead   *wbcrud.Obj
}

// parseScoreBody validates the model like pydantic: both fields are checked and every error reported.
// An integer too large for int64 is a valid Python int; it is clamped, which no lead can ever match.
func parseScoreBody(raw []byte) (scoreBody, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return scoreBody{}, errs
	}
	var out scoreBody
	if v, ok := obj.Get("lead_id"); ok && v != nil {
		n, fe := utilLaxInt(v, []any{"body", "lead_id"})
		if fe != nil {
			errs = append(errs, *fe)
		} else {
			out.LeadID = &n
		}
	}
	if v, ok := obj.Get("lead"); ok && v != nil {
		if o, isObj := v.(*wbcrud.Obj); isObj {
			out.Lead = o
		} else {
			errs = append(errs, httpx.FieldError{Type: "dict_type", Loc: []any{"body", "lead"}, Msg: "Input should be a valid dictionary", Input: v})
		}
	}
	if errs != nil {
		return scoreBody{}, errs
	}
	return out, nil
}

// utilLaxInt is pydantic's lax int for Optional[int]: ints, integral floats, bools and numeric strings.
func utilLaxInt(v any, loc []any) (int64, *httpx.FieldError) {
	parsing := func() (int64, *httpx.FieldError) {
		return 0, &httpx.FieldError{Type: "int_parsing", Loc: loc, Msg: "Input should be a valid integer, unable to parse string as an integer", Input: v}
	}
	clamp := func(neg bool) (int64, *httpx.FieldError) {
		if neg {
			return math.MinInt64, nil
		}
		return math.MaxInt64, nil
	}
	switch t := v.(type) {
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return clamp(strings.HasPrefix(s, "-"))
			}
			return n, nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return parsing()
		}
		if f != math.Trunc(f) {
			return 0, &httpx.FieldError{Type: "int_from_float", Loc: loc, Msg: "Input should be a valid integer, got a number with a fractional part", Input: v}
		}
		if math.Abs(f) >= 1<<63 {
			return clamp(f < 0)
		}
		return int64(f), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		s := strings.TrimSpace(t)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		} else if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return clamp(strings.HasPrefix(s, "-"))
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) && !math.IsInf(f, 0) {
			if math.Abs(f) >= 1<<63 {
				return clamp(f < 0)
			}
			return int64(f), nil
		}
		return parsing()
	}
	return 0, &httpx.FieldError{Type: "int_type", Loc: loc, Msg: "Input should be a valid integer", Input: v}
}

// parseStringField validates a body model with one required str field (VerifyEmailBody, TechStackBody).
func parseStringField(raw []byte, field string) (string, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return "", errs
	}
	loc := []any{"body", field}
	v, ok := obj.Get(field)
	if !ok {
		return "", []httpx.FieldError{{Type: "missing", Loc: loc, Msg: "Field required", Input: obj}}
	}
	s, isStr := v.(string)
	if !isStr {
		return "", []httpx.FieldError{{Type: "string_type", Loc: loc, Msg: "Input should be a valid string", Input: v}}
	}
	return s, nil
}

// plainValue turns a decoded JSON value into the plain Go types leadscore reads.
func plainValue(v any) any {
	switch t := v.(type) {
	case *wbcrud.Obj:
		m := make(map[string]any, t.Len())
		for _, k := range t.Keys() {
			x, _ := t.Get(k)
			m[k] = plainValue(x)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = plainValue(x)
		}
		return out
	}
	return v
}

// scoreInline is score_lead(Lead.from_dict(lead)): unknown keys are never read, an absent company is the
// dataclass default "" and a wrong-typed field is scored exactly as Python would (no coercion). Where Python
// raises (a bare 500) the *leadscore.PyError is returned so the route answers 422.
func scoreInline(lead *wbcrud.Obj) (company any, score int, err error) {
	m := make(leadscore.MapLead, lead.Len())
	for _, k := range lead.Keys() {
		v, _ := lead.Get(k)
		m[k] = plainValue(v)
	}
	company = ""
	if v, ok := lead.Get("company"); ok {
		company = v
	}
	score, err = leadscore.ScoreE(m, nil)
	return company, score, err
}

// score is POST /api/leads/score: lead_id (looked up in the caller's workspace) wins over an inline lead.
func (h *leadUtils) score(w http.ResponseWriter, r *http.Request) {
	raw, ok := readUtilBody(w, r, maxScoreBody)
	if !ok {
		return
	}
	body, errs := parseScoreBody(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var company any
	var score int
	switch {
	case body.LeadID != nil:
		wc, _ := identity.WorkspaceFrom(r.Context())
		found := false
		err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) error {
			ls, err := leadsread.LeadsByIDs(r.Context(), tx, []int64{*body.LeadID})
			if err != nil || len(ls) == 0 {
				return err
			}
			found = true
			company, _ = ls[0].Get("company")
			score, err = leadscore.ScoreE(ls[0], nil)
			return err
		})
		if err != nil {
			h.d.logger().Error("leadutils: score failed", "err_type", errTypeName(err))
			serverError(w)
			return
		}
		if !found {
			httpx.Detail(w, http.StatusNotFound, "Lead not found")
			return
		}
	case body.Lead != nil && body.Lead.Len() > 0:
		var err error
		company, score, err = scoreInline(body.Lead)
		var pe *leadscore.PyError
		if errors.As(err, &pe) {
			// Python answers a bare 500 (TypeError/AttributeError inside score_lead); a wrong-typed field is
			// the client's mistake, so Go answers a 422 that names the field's problem, no internals.
			httpx.ValidationError(w, httpx.FieldError{Type: "value_error", Loc: []any{"body", "lead"},
				Msg: "Value error, lead has a field of the wrong type: " + pe.Msg, Input: body.Lead})
			return
		} else if err != nil {
			serverError(w)
			return
		}
	default:
		httpx.Detail(w, http.StatusBadRequest, "provide lead_id or lead")
		return
	}
	out := wbcrud.NewObj()
	if body.LeadID != nil {
		out.Set("lead_id", json.Number(strconv.FormatInt(*body.LeadID, 10)))
	} else {
		out.Set("lead_id", nil)
	}
	out.Set("company", company)
	out.Set("score", score)
	out.Set("tier", leadscore.Tier(score))
	httpx.WriteJSON(w, http.StatusOK, out)
}
