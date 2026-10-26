package api

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destinbound"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// The public CRM callback is unauthenticated by session: the destination-bound inbound token is the only credential, the
// workspace comes only from that token's row, and a refusal never says whether a destination or workspace exists (one 401
// text for every authentication failure, as in Python). Hardening beyond Python (D5): 120 requests per minute per client IP
// and per verified token, a 1 MiB body cap, and the real client IP from the trusted proxy header.
const (
	inboundMaxBody  = 1 << 20
	inboundRateSpec = "120/minute"
	inboundFieldMax = 10_000
)

type inboundRoutes struct {
	d     Deps
	pub   *pubRoutes // clientIP: the trusted-proxy aware client address of the other public routes
	ipMW  httpx.Middleware
	tokMW httpx.Middleware
}

// inboundPath is the route; the destination id is its last segment.
const inboundPath = drPrefix + "/inbound/"

// registerDestsInbound serves POST /api/audience-destinations/inbound/{destination_id} (Task 21): public, no authentication
// or CSRF middleware.
//
// It is not a mux pattern. "POST .../inbound/{destination_id}" overlaps "POST .../{destination_id}/sync" and
// "POST .../{destination_id}/inbound-token" (both match ".../inbound/sync"), and ServeMux panics at registration when two
// patterns overlap and neither is more specific. FastAPI resolves such a pair by registration order, so the route is served
// by the innermost global middleware, which takes exactly POST <inboundPath><one segment> and hands everything else on. As in
// Python (sync and inbound-token are declared first) the two overlapping literals still belong to those routes.
func registerDestsInbound(r *httpx.Router, d Deps) {
	lim := ratelimit.New(nil)
	h := &inboundRoutes{d: d,
		pub:   &pubRoutes{d: d, proxies: parseTrustedProxies(d.getenv()("TRUST_PROXY_CIDRS"), d.logger())},
		ipMW:  lim.Limit(inboundRateSpec), // each Limit call has its own budget
		tokMW: lim.Limit(inboundRateSpec)}
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if id, ok := inboundDestination(req); ok {
				h.callback(w, req, id)
				return
			}
			next.ServeHTTP(w, req)
		})
	})
}

// inboundDestination reports whether req is POST <inboundPath>{destination_id} and returns the decoded id.
func inboundDestination(req *http.Request) (string, bool) {
	if req.Method != http.MethodPost {
		return "", false
	}
	rest, ok := strings.CutPrefix(req.URL.EscapedPath(), inboundPath)
	if !ok || rest == "" || strings.Contains(rest, "/") || rest == "sync" || rest == "inbound-token" {
		return "", false
	}
	id, err := url.PathUnescape(rest)
	if err != nil || id == "" {
		return "", false
	}
	return id, true
}

// limited runs next unless key is over the budget of mw (429, the shared limiter's body). The limiter is handed our key through the
// request context; the client's own headers never choose a bucket.
func limited(mw httpx.Middleware, key string, w http.ResponseWriter, r *http.Request, next func()) {
	r2 := r.WithContext(ratelimit.WithKey(r.Context(), key))
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { next() })).ServeHTTP(w, r2)
}

func (h *inboundRoutes) callback(w http.ResponseWriter, r *http.Request, destID string) {
	// The IP budget is spent first, before anything is read or checked.
	limited(h.ipMW, "ip:"+h.pub.clientIP(r), w, r, func() { h.process(w, r, destID) })
}

// bearerToken is Python's authorization[7:].strip() when the header starts with "bearer " (any case), else "".
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// errInboundNotFound is the 404 for a destination that is missing, not a CRM, or disabled.
var errInboundNotFound = errors.New("active CRM destination not found")

func (h *inboundRoutes) process(w http.ResponseWriter, r *http.Request, destID string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, inboundMaxBody+1))
	if err != nil {
		httpx.Detail(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(raw) > inboundMaxBody {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	// FastAPI validates the body before the handler authenticates, so a malformed body is a 422 whatever the token.
	body, errs := parseInboundEvent(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	token := bearerToken(r)
	ws, err := destinbound.Resolve(r.Context(), h.d.Pool, token, destID, time.Now())
	if errors.Is(err, destinbound.ErrInvalidToken) {
		httpx.Detail(w, http.StatusUnauthorized, err.Error(), httpx.Header{Key: "WWW-Authenticate", Value: "Bearer"})
		return
	}
	if err != nil {
		h.pub.fail(w, "inbound resolve", err)
		return
	}
	// The token budget is spent only by a token that verified and is keyed by its digest: junk sharing a real token's prefix
	// cannot exhaust that token's budget.
	limited(h.tokMW, "tok:"+destinbound.HashToken(token), w, r, func() { h.reconcile(w, r, ws, destID, body) })
}

func (h *inboundRoutes) reconcile(w http.ResponseWriter, r *http.Request, ws, destID string, body destinbound.InboundBody) {
	var receipt destinbound.Receipt
	var replay bool
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		// Reconcile's concurrent-replay handling is written for READ COMMITTED; refuse to run under anything else.
		var iso string
		if err := tx.QueryRow(r.Context(), `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
			return err
		}
		if iso != "read committed" {
			return errors.New("inbound callback requires READ COMMITTED, transaction is " + iso)
		}
		row, err := gen.New(tx).DestGet(r.Context(), gen.DestGetParams{WorkspaceID: ws, ID: destID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errInboundNotFound
		}
		if err != nil {
			return err
		}
		if (row.DestinationType != "hubspot" && row.DestinationType != "salesforce") || !row.Enabled {
			return errInboundNotFound
		}
		receipt, replay, err = destinbound.Reconcile(r.Context(), tx, destinbound.Destination{
			ID: row.ID, WorkspaceID: row.WorkspaceID, DestinationType: row.DestinationType,
			Config: row.Config, FieldMap: row.FieldMap}, body)
		return err
	})
	switch {
	case errors.Is(err, errInboundNotFound):
		httpx.Detail(w, http.StatusNotFound, "Active CRM destination not found")
		return
	case errors.Is(err, destinbound.ErrEventIDRequired):
		httpx.Detail(w, http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, destinbound.ErrInvalidBody):
		// a lead id outside the integer column: Python's query fails (500); here a plain refusal
		httpx.Detail(w, http.StatusUnprocessableEntity, "invalid callback body")
		return
	case err != nil:
		h.pub.fail(w, "inbound reconcile", err)
		return
	}
	out, err := json.Marshal(receipt)
	if err != nil {
		h.pub.fail(w, "inbound marshal", err)
		return
	}
	// {**receipt, "replay": replay}: replay is the last key.
	out = append(out[:len(out)-1], []byte(`,"replay":`+map[bool]string{true: "true", false: "false"}[replay]+"}")...)
	httpx.WriteJSON(w, http.StatusOK, json.RawMessage(out))
}

func valueError(msg string, at []any, input any) httpx.FieldError {
	return httpx.FieldError{Type: "value_error", Loc: at, Msg: "Value error, " + msg, Input: input,
		Ctx: map[string]any{"error": map[string]any{}}}
}

// parseInboundEvent validates InboundEvent (pydantic v2 / FastAPI error shapes, every field reported in model order) and
// returns the normalised body for Reconcile.
func parseInboundEvent(raw []byte) (destinbound.InboundBody, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return destinbound.InboundBody{}, errs
	}
	var eventID string
	var recordID *string
	var leadID *int64
	fields := wbcrud.NewObj()
	add := func(e *httpx.FieldError) {
		if e != nil {
			errs = append(errs, *e)
		}
	}

	if v, ok := obj.Get("external_event_id"); !ok {
		add(&httpx.FieldError{Type: "missing", Loc: seqLoc("external_event_id"), Msg: "Field required", Input: obj})
	} else if s, e := seqString(v, seqLoc("external_event_id"), 255); e != nil {
		add(e)
	} else if s == "" {
		add(&httpx.FieldError{Type: "string_too_short", Loc: seqLoc("external_event_id"), Input: v,
			Msg: "String should have at least 1 character", Ctx: map[string]any{"min_length": 1}})
	} else {
		eventID = s
	}
	if v, ok := obj.Get("external_record_id"); ok && v != nil {
		if s, e := seqString(v, seqLoc("external_record_id"), 255); e != nil {
			add(e)
		} else {
			recordID = &s
		}
	}
	if v, ok := obj.Get("lead_id"); ok && v != nil {
		if n, e := seqIntRange(v, seqLoc("lead_id"), 1, math.MaxInt64); e != nil {
			add(e)
		} else {
			leadID = &n
		}
	}
	if v, ok := obj.Get("fields"); ok {
		at := seqLoc("fields")
		f, e := watchDict(v, at)
		switch {
		case e != nil:
			add(e)
		case f.Len() > 50:
			add(ptr(valueError("fields may contain at most 50 entries", at, f)))
		default:
			fields = f
			add(inboundFieldsCheck(f, at))
		}
	}
	if errs != nil {
		return destinbound.InboundBody{}, errs
	}

	// Re-encode the validated values (leading key order kept) for the ordered, canonical decode Reconcile expects.
	fj, err := json.Marshal(fields)
	if err != nil {
		return destinbound.InboundBody{}, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	norm, err := json.Marshal(struct {
		E string          `json:"external_event_id"`
		R *string         `json:"external_record_id"`
		L *int64          `json:"lead_id"`
		F json.RawMessage `json:"fields"`
	}{eventID, recordID, leadID, fj})
	if err != nil {
		return destinbound.InboundBody{}, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	body, err := destinbound.ParseInboundBody(norm)
	if err != nil {
		return destinbound.InboundBody{}, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	body.FillModelDefaults()
	return body, nil
}

func ptr[T any](v T) *T { return &v }

// inboundFieldsCheck is the rest of bounded_fields: scalar values only, each at most 10000 characters (str(item), None exempt).
func inboundFieldsCheck(f *wbcrud.Obj, at []any) *httpx.FieldError {
	keys := f.Keys()
	for _, k := range keys {
		switch v, _ := f.Get(k); v.(type) {
		case *wbcrud.Obj, []any:
			e := valueError("field values must be scalar", at, f)
			return &e
		}
	}
	for _, k := range keys {
		v, _ := f.Get(k)
		n := 0
		switch t := v.(type) {
		case nil:
			continue
		case string:
			n = utf8.RuneCountInString(t)
		case interface{ String() string }: // json.Number
			n = len(t.String())
		}
		if n > inboundFieldMax {
			e := valueError("field values may not exceed 10000 characters", at, f)
			return &e
		}
	}
	return nil
}
