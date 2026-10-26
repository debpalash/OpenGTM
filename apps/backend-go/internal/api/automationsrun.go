package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoengine"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// maxAutoRunBody bounds the preview and run bodies (Python has no limit; a body is a short id list).
const maxAutoRunBody = 1 << 20

// autoRunNow is the clock of the manual-run fire key (tests pin it).
var autoRunNow = func() time.Time { return time.Now().UTC() }

// manualDryRun is the one place that decides what a manual run's dry_run request means
// for the enqueued job. Python parity (a product decision is pending): the flag is
// passed through, and the worker still executes the actions on a dry run while it
// persists nothing. Flipping to a side-effect-free dry run is a change here (force the
// flag on the payload) together with the single dry-run branch in autoengine.
func manualDryRun(requested bool) bool { return requested }

// registerAutomationsRun registers the automations preview, manual run and run
// history routes (routers/automations.py): preview, runs and run detail are workspace
// reads, run needs automations.manage.
func registerAutomationsRun(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Identity == nil {
		return
	}
	h := autoRunRoutes{d: d, svc: &autosvc.Service{Pool: d.Pool, ExecFor: d.automationExecutor(), Log: d.Log,
		Now: func() time.Time { return autoRunNow() }}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	r.Handle("POST /api/automations/triggers/{trigger_id}/preview", http.HandlerFunc(h.preview), authn, workspace)
}

type autoRunRoutes struct {
	d   Deps
	svc *autosvc.Service
}

// disabled answers 404 `automations disabled` (Python _require_enabled) after the
// request has been validated.
func (h autoRunRoutes) disabled(w http.ResponseWriter) bool {
	if h.d.Flags.AutomationsEnabled {
		return false
	}
	httpx.Detail(w, http.StatusNotFound, "automations disabled")
	return true
}

func (h autoRunRoutes) fail(w http.ResponseWriter, what string, err error) {
	var he *autosvc.HTTPError
	if errors.As(err, &he) {
		httpx.WriteJSON(w, he.Status, map[string]any{"detail": he.Detail})
		return
	}
	if errors.Is(err, autoengine.ErrPreviewNoTrigger) {
		httpx.Detail(w, http.StatusNotFound, "trigger not found")
		return
	}
	l := h.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("automations: "+what+" failed", "err_type", fmt.Sprintf("%T", err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// autoRunBody is a validated PreviewRequest / RunRequest.
type autoRunBody struct {
	rowIDs []string
	limit  int64 // PreviewRequest.limit (25)
	dryRun bool  // RunRequest.dry_run (false)
}

// parseAutoRunBody validates `row_ids: Optional[list[str]] = None` plus `limit: int = 25`
// (preview) or `dry_run: bool = False` (run), with pydantic's messages and locs.
func parseAutoRunBody(raw []byte, preview bool) (autoRunBody, []httpx.FieldError) {
	out := autoRunBody{limit: 25}
	doc, errs := wbcrud.DecodeBody(raw)
	if errs != nil {
		return out, errs
	}
	o, errs := wbcrud.PydModel(doc)
	if errs != nil {
		return out, errs
	}
	if v, ok := o.Get("row_ids"); ok && v != nil {
		list, isList := v.([]any)
		if !isList {
			errs = append(errs, wbcrud.PydError("list_type", "Input should be a valid list", []any{"body", "row_ids"}, v)...)
		}
		for i, e := range list {
			s, isStr := e.(string)
			if !isStr {
				errs = append(errs, wbcrud.PydError("string_type", "Input should be a valid string", []any{"body", "row_ids", i}, e)...)
				continue
			}
			out.rowIDs = append(out.rowIDs, s)
		}
	}
	if preview {
		if v, ok := o.Get("limit"); ok {
			n, _, e := wbcrud.PydInt(v, []any{"body", "limit"})
			if e != nil {
				errs = append(errs, e...)
			}
			out.limit = n
		}
	} else if v, ok := o.Get("dry_run"); ok {
		b, e := wbcrud.PydBool(v, []any{"body", "dry_run"})
		if e != nil {
			errs = append(errs, e...)
		}
		out.dryRun = b
	}
	if errs != nil {
		return out, errs
	}
	return out, nil
}

func (h autoRunRoutes) readBody(w http.ResponseWriter, r *http.Request, preview bool) (autoRunBody, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAutoRunBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		} else {
			httpx.Detail(w, http.StatusBadRequest, "Request body could not be read")
		}
		return autoRunBody{}, false
	}
	b, errs := parseAutoRunBody(raw, preview)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return autoRunBody{}, false
	}
	return b, true
}

// preview is POST /triggers/{id}/preview. It reads only: the transaction is read-only
// (so a write would fail, not just be absent) and nothing here has an HTTP client.
func (h autoRunRoutes) preview(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	ws, id := wc.WorkspaceID, r.PathValue("trigger_id")
	body, ok := h.readBody(w, r, true)
	if !ok || h.disabled(w) {
		return
	}
	limit := autoengine.ClampPreviewLimit(body.limit)
	var out any
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), "SET TRANSACTION READ ONLY"); err != nil {
			return err
		}
		res, err := autoengine.Preview(r.Context(), tx, ws, id, body.rowIDs, limit, h.d.Flags.Automations.MaxRowsPerEval, autoRunNow())
		out = res
		return err
	})
	if err != nil {
		h.fail(w, "preview", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
