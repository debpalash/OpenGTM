package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/entities"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// RegisterEntities registers /api/entities (read routes; merge/split live in entitiesroutes_write.go).
func RegisterEntities(r *httpx.Router, d Deps) {
	e := entityRoutes{d: d, st: &entities.Store{Pool: d.Pool}}
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	r.Handle("GET /api/entities/company", http.HandlerFunc(e.list), mws...)
	r.Handle("GET /api/entities/company/{entity_id}", http.HandlerFunc(e.get), mws...)
	r.Handle("GET /api/entities/review-queue", http.HandlerFunc(e.reviewQueue), mws...)
}

type entityRoutes struct {
	d  Deps
	st *entities.Store
}

func (e entityRoutes) fail(w http.ResponseWriter, what string, err error) {
	l := e.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("entities: "+what+" failed", "err", err)
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// queryInt parses an optional int query parameter like FastAPI: default when absent,
// 422 int_parsing when not an integer, and a ge/le bound (nil = unbounded).
func queryInt(w http.ResponseWriter, r *http.Request, name string, def int, ge, le *int) (int, bool) {
	vals, present := r.URL.Query()[name]
	if !present {
		return def, true
	}
	raw := vals[len(vals)-1]
	loc := []any{"query", name}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < -2147483648 || n > 2147483647 {
		httpx.ValidationError(w, httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
			Msg: "Input should be a valid integer, unable to parse string as an integer"})
		return 0, false
	}
	if ge != nil && n < *ge {
		httpx.ValidationError(w, httpx.FieldError{Type: "greater_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be greater than or equal to " + strconv.Itoa(*ge), Ctx: map[string]any{"ge": *ge}})
		return 0, false
	}
	if le != nil && n > *le {
		httpx.ValidationError(w, httpx.FieldError{Type: "less_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be less than or equal to " + strconv.Itoa(*le), Ctx: map[string]any{"le": *le}})
		return 0, false
	}
	return n, true
}

func intp(n int) *int { return &n }

func (e entityRoutes) list(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	// Python validates query params in declaration order; the first failure of each is reported together,
	// but a single failing parameter is the contract surface exercised here.
	minC, ok := queryInt(w, r, "min_corroboration", 1, intp(1), nil)
	if !ok {
		return
	}
	limit, ok := queryInt(w, r, "limit", 100, nil, intp(1000))
	if !ok {
		return
	}
	out, err := e.st.ListCompanies(r.Context(), wc.WorkspaceID, int32(minC), int32(limit))
	if err != nil {
		e.fail(w, "list companies", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"entities": out})
}

func (e entityRoutes) get(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	c, err := e.st.GetCompany(r.Context(), wc.WorkspaceID, r.PathValue("entity_id"))
	if errors.Is(err, entities.ErrNotFound) {
		httpx.Detail(w, http.StatusNotFound, "Entity not found")
		return
	}
	if err != nil {
		e.fail(w, "get company", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (e entityRoutes) reviewQueue(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	status := "pending"
	if v, ok := r.URL.Query()["status"]; ok {
		status = v[len(v)-1]
	}
	limit, ok := queryInt(w, r, "limit", 100, nil, intp(1000))
	if !ok {
		return
	}
	out, err := e.st.ReviewQueue(r.Context(), wc.WorkspaceID, status, int32(limit))
	if err != nil {
		e.fail(w, "review queue", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"pairs": out})
}
