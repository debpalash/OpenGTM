package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Hardening bounds (D4). Python stores whatever normalizes to a non-empty string and tries to store an empty key for
// a blank address; Go answers a pydantic-shaped 422 for a blank or over-long address.
const (
	suppListLimit = 500
	suppEmailMax  = 320 // RFC 5321 path limit
	suppNotFound  = "Suppression not found"
	suppLocked    = "Cannot remove an unsubscribe/complaint suppression"
)

type suppRoutes struct{ seqRoutes }

// registerOutreachSuppressions registers the 3 suppression routes (Task 23). Listing needs any member
// (current_workspace); add and remove need outreach.write (admin).
func registerOutreachSuppressions(r *httpx.Router, d Deps) {
	h := suppRoutes{seqRoutes{d: d}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	read := []httpx.Middleware{authn, identity.Workspace(d.Identity)}
	write := []httpx.Middleware{authn, identity.Workspace(d.Identity), identity.RequireRole("outreach.write", "admin")}
	r.Handle("GET /api/outreach/suppressions", http.HandlerFunc(h.list), read...)
	r.Handle("POST /api/outreach/suppressions", http.HandlerFunc(h.add), write...)
	r.Handle("DELETE /api/outreach/suppressions/{email}", http.HandlerFunc(h.remove), write...)
}

func (h suppRoutes) list(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	var rows []outreachstore.Suppression
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		rows, e = outreachstore.ListSuppressions(r.Context(), tx, suppListLimit)
		return e
	})
	if err != nil {
		h.fail(w, "suppressions list", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"suppressions": rows})
}

// parseSuppressionAdd validates SuppressionAdd{email: str}, plus the hardening bounds: at most 320 characters and
// not blank once normalized.
func parseSuppressionAdd(raw []byte) (string, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return "", errs
	}
	loc := seqLoc("email")
	v, present := obj.Get("email")
	if !present {
		return "", []httpx.FieldError{seqFE("missing", "Field required", loc, obj, nil)}
	}
	s, e := seqString(v, loc, suppEmailMax)
	if e != nil {
		return "", []httpx.FieldError{*e}
	}
	if emailnorm.Normalize(s) == "" {
		return "", []httpx.FieldError{seqFE("value_error", "Value error, email must not be blank", loc, v, nil)}
	}
	return s, nil
}

func (h suppRoutes) add(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	email, errs := parseSuppressionAdd(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var added bool
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		added, e = outreachstore.AddSuppression(r.Context(), tx, ws, email, "manual", "manual", false)
		return e
	})
	if err != nil {
		h.fail(w, "suppressions add", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		Added  bool   `json:"added"`
	}{"ok", added})
}

func (h suppRoutes) remove(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	email := r.PathValue("email")    // already percent-decoded; normalized by the store like Python
	if len(email) > 4*suppEmailMax { // no stored address is this long: skip the query
		httpx.Detail(w, http.StatusNotFound, suppNotFound)
		return
	}
	var out outreachstore.Outcome
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		out, e = outreachstore.RemoveSuppression(r.Context(), tx, ws, email)
		return e
	})
	if err != nil {
		h.fail(w, "suppressions remove", err)
		return
	}
	switch out {
	case outreachstore.OutcomeNotFound:
		httpx.Detail(w, http.StatusNotFound, suppNotFound)
	case outreachstore.OutcomeLocked:
		httpx.Detail(w, http.StatusForbidden, suppLocked)
	default:
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
