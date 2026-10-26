package api

import (
	"errors"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/entities"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// registerEntityWrites registers the editor-gated entity routes (require_editor in Python).
func registerEntityWrites(r *httpx.Router, d Deps, e entityRoutes) {
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin")}
	r.Handle("POST /api/entities/merge", http.HandlerFunc(e.merge), mws...)
	r.Handle("POST /api/entities/split", http.HandlerFunc(e.split), mws...)
	r.Handle("POST /api/entities/review-queue/decide", http.HandlerFunc(e.decide), mws...)
}

// intField validates a required int body field like pydantic (lax mode).
func intField(obj map[string]any, key string, errs *[]httpx.FieldError) (pyIntVal, bool) {
	v, present := obj[key]
	if !present {
		*errs = append(*errs, httpx.FieldError{Type: "missing", Loc: []any{"body", key}, Msg: "Field required", Input: obj})
		return pyIntVal{}, false
	}
	if b, isBool := v.(bool); isBool { // pydantic lax mode reads JSON true/false as 1/0
		if b {
			return pyIntVal{v: 1}, true
		}
		return pyIntVal{}, true
	}
	n, fe := pyInt(v, []any{"body", key})
	if fe != nil {
		*errs = append(*errs, httpx.FieldError{Type: fe.Type, Loc: fe.Loc, Msg: fe.Msg, Input: fe.Input, Ctx: fe.Ctx})
		return pyIntVal{}, false
	}
	return n, true
}

// opFailure maps the store's {"error": ...} answers onto FastAPI's responses.
func (e entityRoutes) opFailure(w http.ResponseWriter, what string, err error) {
	var oe *entities.OpError
	switch {
	case errors.As(err, &oe):
		httpx.Detail(w, http.StatusBadRequest, oe.Code)
	case errors.Is(err, entities.ErrPairNotFound):
		httpx.Detail(w, http.StatusNotFound, "Review pair not found")
	case errors.Is(err, entities.ErrNoCandidate):
		httpx.Detail(w, http.StatusBadRequest, "No candidate entity to merge")
	default:
		e.fail(w, what, err)
	}
}

func (e entityRoutes) merge(w http.ResponseWriter, r *http.Request) {
	obj, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	var errs []httpx.FieldError
	kept := fieldString(obj, "kept_id", true, "", &errs)
	merged := fieldString(obj, "merged_id", true, "", &errs)
	reason := fieldString(obj, "reason", false, "manual", &errs)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	res, err := e.st.Merge(r.Context(), wc.WorkspaceID, kept, merged, reason)
	if err != nil {
		e.opFailure(w, "merge", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (e entityRoutes) split(w http.ResponseWriter, r *http.Request) {
	obj, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	var errs []httpx.FieldError
	id, _ := intField(obj, "merge_log_id", &errs)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	logID := id.v
	if id.huge {
		logID = 1 << 40 // beyond int4: no such log
	}
	res, err := e.st.Split(r.Context(), wc.WorkspaceID, logID)
	if err != nil {
		e.opFailure(w, "split", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (e entityRoutes) decide(w http.ResponseWriter, r *http.Request) {
	obj, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	var errs []httpx.FieldError
	pair, _ := intField(obj, "pair_id", &errs)
	decision := fieldString(obj, "decision", true, "", &errs)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	pairID := pair.v
	if pair.huge {
		pairID = 1 << 40
	}
	res, err := e.st.Decide(r.Context(), wc.WorkspaceID, pairID, decision)
	if err != nil {
		e.opFailure(w, "decide", err)
		return
	}
	if res.Merge == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": res.Status})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		entities.MergeResult
	}{res.Status, *res.Merge})
}
