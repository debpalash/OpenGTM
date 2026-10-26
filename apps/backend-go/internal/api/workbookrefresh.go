package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/refresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// maxRefreshPolicyBody bounds PUT .../refresh-policy (Python has no limit; a policy is a few KB).
const maxRefreshPolicyBody = 1 << 20

// registerWorkbookRefresh registers PUT .../refresh-policy and POST .../refresh
// (routers/workbooks.py update_refresh_policy / refresh_now), both require_editor.
func registerWorkbookRefresh(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Identity == nil || !d.workbooksEnabled("workbook refresh") {
		return
	}
	h := refreshRoutes{d: d, svc: &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("tables.write", "editor", "admin")
	r.Handle("PUT /api/workbooks/{workbook_id}/refresh-policy", http.HandlerFunc(h.policy), authn, workspace, editor)
	r.Handle("POST /api/workbooks/{workbook_id}/refresh", http.HandlerFunc(h.refreshNow), authn, workspace, editor)
}

type refreshRoutes struct {
	d   Deps
	svc *wbcrud.Service
}

// executor is go_executor.refresh_executor: the same cohort resolution the
// worker applies when it chains the next refresh (config.ExecutorSettings).
func (h refreshRoutes) executor(ws string) string {
	return config.ExecutorCohortFrom(h.d.getenv()).RefreshExecutor(ws)
}

func (h refreshRoutes) fail(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, wbcrud.ErrNotFound) {
		httpx.Detail(w, http.StatusNotFound, wbcrud.NotFoundDetail)
		return
	}
	l := h.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("workbook refresh: "+what+" failed", "err_type", fmt.Sprintf("%T", err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// refreshPolicyLoc is the pydantic loc of a RefreshPolicyRequest field.
func refreshPolicyLoc(name string) []any { return []any{"body", name} }

// parseRefreshPolicy validates RefreshPolicyRequest: enabled bool = True,
// interval Optional[str], on_signal list = [], staleness_ttl_days dict = {}.
// The result is model_dump() (field order enabled, interval, on_signal,
// staleness_ttl_days).
func parseRefreshPolicy(raw []byte) (*wbcrud.Obj, []httpx.FieldError) {
	doc, errs := wbcrud.DecodeBody(raw)
	if errs != nil {
		return nil, errs
	}
	o, errs := wbcrud.PydModel(doc)
	if errs != nil {
		return nil, errs
	}
	enabled, interval, onSignal, ttl := any(true), any(nil), any([]any{}), any(wbcrud.NewObj())
	if v, ok := o.Get("enabled"); ok {
		b, e := wbcrud.PydBool(v, refreshPolicyLoc("enabled"))
		if e != nil {
			errs = append(errs, e...)
		}
		enabled = b
	}
	if v, ok := o.Get("interval"); ok && v != nil {
		if _, isStr := v.(string); !isStr {
			errs = append(errs, wbcrud.PydError("string_type", "Input should be a valid string", refreshPolicyLoc("interval"), v)...)
		}
		interval = v
	}
	if v, ok := o.Get("on_signal"); ok {
		if _, isList := v.([]any); !isList {
			errs = append(errs, wbcrud.PydError("list_type", "Input should be a valid list", refreshPolicyLoc("on_signal"), v)...)
		}
		onSignal = v
	}
	if v, ok := o.Get("staleness_ttl_days"); ok {
		if _, isObj := v.(*wbcrud.Obj); !isObj {
			errs = append(errs, wbcrud.PydError("dict_type", "Input should be a valid dictionary", refreshPolicyLoc("staleness_ttl_days"), v)...)
		}
		ttl = v
	}
	if errs != nil {
		return nil, errs
	}
	p := wbcrud.NewObj()
	p.Set("enabled", enabled)
	p.Set("interval", interval)
	p.Set("on_signal", onSignal)
	p.Set("staleness_ttl_days", ttl)
	return p, nil
}

func (h refreshRoutes) policy(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	ws, id := wc.WorkspaceID, r.PathValue("workbook_id")
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRefreshPolicyBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		} else {
			httpx.Detail(w, http.StatusBadRequest, "Request body could not be read")
		}
		return
	}
	pol, errs := parseRefreshPolicy(raw)
	if errs != nil {
		// FastAPI validates the body before the dependency-free handler runs, but after
		// the require_editor dependency: the gates above already answered 401/403.
		httpx.ValidationError(w, errs...)
		return
	}
	policy := map[string]any{}
	for _, k := range pol.Keys() {
		policy[k], _ = pol.Get(k)
	}
	var result map[string]any
	err = h.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		if _, err := h.svc.Owned(r.Context(), tx, ws, id, false); err != nil {
			return err
		}
		result, err = refresh.SetPolicyExecutor(r.Context(), tx, id, policy, h.executor(ws))
		return err
	})
	if err != nil {
		h.fail(w, "policy", err)
		return
	}
	if e, bad := result["error"]; bad {
		msg, _ := e.(string)
		httpx.Detail(w, http.StatusNotFound, msg)
		return
	}
	out := wbcrud.NewObj()
	out.Set("refresh_policy", pol)
	out.Set("next_in_minutes", result["next_in_minutes"])
	httpx.WriteJSON(w, http.StatusOK, out)
}

// refreshNowPayload is {"workbook_id", "reason", "workspace_id"} in Python's key order.
type refreshNowPayload struct {
	WorkbookID  string `json:"workbook_id"`
	Reason      string `json:"reason"`
	WorkspaceID string `json:"workspace_id"`
}

func (h refreshRoutes) refreshNow(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	ws, id := wc.WorkspaceID, r.PathValue("workbook_id")
	err := h.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		_, err := h.svc.Owned(r.Context(), tx, ws, id, false)
		if err != nil {
			return err
		}
		// OD-4: stamp the workbook's tenant (Owned already filtered by ws) so the refresh worker scopes correctly.
		payload, err := json.Marshal(refreshNowPayload{WorkbookID: id, Reason: "manual", WorkspaceID: ws})
		if err != nil {
			return err
		}
		_, err = gen.New(tx).EnqueueRefreshNowJob(r.Context(), gen.EnqueueRefreshNowJobParams{
			Executor: h.executor(ws), Payload: payload, WorkspaceID: ws,
		})
		return err
	})
	if err != nil {
		h.fail(w, "refresh", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{"refreshing"})
}
