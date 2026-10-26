package api

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govadmin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govaudit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

type auditRoutes struct{ d Deps }

// registerGovernanceAudit registers the audit-events list and CSV export (Task 12). Both need
// governance.manage (admin), as Python's require_admin.
func registerGovernanceAudit(r *httpx.Router, d Deps) {
	h := auditRoutes{d: d}
	mw := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity), govadmin.Gate("governance.manage")}
	r.Handle("GET /api/governance/audit-events", http.HandlerFunc(h.list), mw...)
	r.Handle("GET /api/governance/audit-events/export.csv", http.HandlerFunc(h.export), mw...)
}

// auditQuery is the parsed filter set. Last value wins for a repeated parameter (FastAPI scalar).
type auditQuery struct {
	filter govaudit.Filter
	cursor string
	limit  int
}

func auditLastQuery(q url.Values, k string) (string, bool) {
	v, ok := q[k]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

// parseAuditQuery applies the router's query validation, collecting every error in declaration order
// (actor_user_id, method, outcome, since, before, cursor, limit). withPaging is false for the export,
// which declares neither cursor nor limit.
func parseAuditQuery(q url.Values, withPaging bool) (auditQuery, []httpx.FieldError) {
	var out auditQuery
	var errs []httpx.FieldError
	if raw, ok := auditLastQuery(q, "actor_user_id"); ok {
		loc := []any{"query", "actor_user_id"}
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		var ne *strconv.NumError
		switch {
		case errors.As(err, &ne) && ne.Err == strconv.ErrSyntax:
			errs = append(errs, httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
				Msg: "Input should be a valid integer, unable to parse string as an integer"})
		case err != nil || n < math.MinInt32:
			// Hardening: Python hands int8 to Postgres (a 500); Go answers a bounded 422.
			errs = append(errs, httpx.FieldError{Type: "greater_than_equal", Loc: loc, Input: raw,
				Msg: "Input should be greater than or equal to -2147483648", Ctx: map[string]any{"ge": int64(math.MinInt32)}})
		case n > math.MaxInt32:
			errs = append(errs, httpx.FieldError{Type: "less_than_equal", Loc: loc, Input: raw,
				Msg: "Input should be less than or equal to 2147483647", Ctx: map[string]any{"le": int64(math.MaxInt32)}})
		default:
			v := int32(n)
			out.filter.ActorUserID = &v
		}
	}
	out.filter.Method, _ = auditLastQuery(q, "method")
	out.filter.Outcome, _ = auditLastQuery(q, "outcome")
	for _, p := range []struct {
		name string
		dst  **time.Time
	}{{"since", &out.filter.Since}, {"before", &out.filter.Before}} {
		raw, ok := auditLastQuery(q, p.name)
		if !ok {
			continue
		}
		t, err := govaudit.ParseTimeParam(raw)
		var te *govaudit.TimeParamError
		if errors.As(err, &te) {
			errs = append(errs, httpx.FieldError{Type: te.Type, Loc: []any{"query", p.name}, Msg: te.Msg, Input: raw,
				Ctx: map[string]any{"error": strings.TrimPrefix(te.Msg, "Input should be a valid datetime or date, ")}})
			continue
		}
		*p.dst = &t
	}
	if !withPaging {
		return out, errs
	}
	out.cursor, _ = auditLastQuery(q, "cursor")
	limRaw, hasLim := auditLastQuery(q, "limit")
	var pe []httpx.FieldError
	out.limit, pe = govaudit.ValidateListParams(out.cursor, limRaw, hasLim)
	return out, append(errs, pe...)
}

func (h auditRoutes) ws(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return "", false
	}
	return c.WorkspaceID, true
}

// fail never logs request data (audit metadata can hold secrets/PII); only the error type.
func (h auditRoutes) fail(w http.ResponseWriter, op string, err error) {
	h.d.logger().Error("governance audit: request failed", "op", op, "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h auditRoutes) list(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	q, errs := parseAuditQuery(r.URL.Query(), true)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var page govaudit.Page
	var cerr error
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		page, cerr = govaudit.List(r.Context(), tx, ws, q.filter, q.cursor, q.limit)
		return nil
	})
	var ce *govaudit.CursorError
	switch {
	case errors.As(cerr, &ce):
		httpx.Detail(w, ce.Status(), ce.Detail)
	case cerr != nil:
		h.fail(w, "list", cerr)
	case err != nil:
		h.fail(w, "list", err)
	default:
		httpx.WriteJSON(w, http.StatusOK, page)
	}
}

func (h auditRoutes) export(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	q, errs := parseAuditQuery(r.URL.Query(), false)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=opengtm-audit-events.csv")
	w.WriteHeader(http.StatusOK)
	// r.Context() ends on client disconnect, which stops the keyset queries. Past the status line a failure can
	// only truncate the stream, as in Python.
	if err := govaudit.ExportCSV(r.Context(), h.d.Pool, ws, q.filter, w); err != nil && r.Context().Err() == nil {
		h.d.logger().Error("governance audit: export aborted", "err_type", errTypeName(err))
	}
}
