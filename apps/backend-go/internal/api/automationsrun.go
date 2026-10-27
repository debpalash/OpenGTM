package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

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
// for the enqueued job. A dry run is preview-only (S-H1): the flag goes on the payload
// and the engine then executes, reserves, debits and sends nothing, writing only
// skipped/dry_run result rows. Python executed the actions on a dry run; that was a bug.
func manualDryRun(requested bool) bool { return requested }

// registerAutomationsRun registers the automations preview, manual run and run
// history routes (routers/automations.py): preview, runs and run detail are workspace
// reads, run needs automations.manage.
func registerAutomationsRun(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Identity == nil {
		// Not silent: preview, run, runs and run-detail would otherwise answer 404 while the CRUD routes stay up.
		d.logger().Error("automations run routes NOT registered: Deps.Pool and Deps.Identity are required")
		return
	}
	h := autoRunRoutes{d: d, svc: &autosvc.Service{Pool: d.Pool, ExecFor: d.automationExecutor(), Log: d.Log,
		Now: func() time.Time { return autoRunNow() }}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	manage := identity.RequireRole("automations.manage", "admin")
	r.Handle("POST /api/automations/triggers/{trigger_id}/preview", http.HandlerFunc(h.preview), authn, workspace)
	r.Handle("POST /api/automations/triggers/{trigger_id}/run", http.HandlerFunc(h.run), authn, workspace, manage)
	r.Handle("GET /api/automations/triggers/{trigger_id}/runs", http.HandlerFunc(h.runs), authn, workspace)
	r.Handle("GET /api/automations/runs/{run_id}", http.HandlerFunc(h.runDetail), authn, workspace)
}

type autoRunRoutes struct {
	d   Deps
	svc *autosvc.Service
}

// badPathID reports an id Postgres cannot store (NUL, invalid UTF-8), which can match
// no row: the route answers 404 before any query (F13) instead of a 500.
func badPathID(id string) bool { return strings.ContainsRune(id, 0) || !utf8.ValidString(id) }

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
	if badPathID(id) {
		httpx.Detail(w, http.StatusNotFound, "trigger not found")
		return
	}
	body, ok := h.readBody(w, r, true)
	if !ok || h.disabled(w) {
		return
	}
	limit := autoengine.ClampPreviewLimit(body.limit)
	var out *wbcrud.Obj
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), "SET TRANSACTION READ ONLY"); err != nil {
			return err
		}
		// S-L3 (final review M2): a caller who cannot manage rules sees webhook URLs masked,
		// as GET and list mask the stored config.
		manage, err := identity.HasPermissionTx(r.Context(), tx, ws, wc.User.ID, "automations.manage", "admin")
		if err != nil {
			return err
		}
		out, err = autoengine.Preview(r.Context(), tx, ws, id, body.rowIDs, limit, h.d.Flags.Automations.MaxRowsPerEval, autoRunNow())
		if err == nil && !manage {
			autoengine.MaskWebhookURLs(out)
		}
		return err
	})
	if err != nil {
		h.fail(w, "preview", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// run is POST /triggers/{id}/run: enqueue one trigger_eval with the cohort executor.
func (h autoRunRoutes) run(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	ws, id := wc.WorkspaceID, r.PathValue("trigger_id")
	if badPathID(id) {
		httpx.Detail(w, http.StatusNotFound, "trigger not found")
		return
	}
	body, ok := h.readBody(w, r, false)
	if !ok || h.disabled(w) {
		return
	}
	// S-L4: explicit targets are bounded by the per-evaluation row cap.
	if max := h.d.Flags.Automations.MaxRowsPerEval; max > 0 && len(body.rowIDs) > max {
		httpx.ValidationError(w, httpx.FieldError{Type: "too_long", Loc: []any{"body", "row_ids"},
			Msg:   fmt.Sprintf("List should have at most %d items after validation, not %d", max, len(body.rowIDs)),
			Input: nil, Ctx: map[string]any{"field_type": "List", "max_length": max, "actual_length": len(body.rowIDs)}})
		return
	}
	dry := manualDryRun(body.dryRun)
	var jobID int64
	var fireKey string
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		var err error
		jobID, fireKey, err = h.svc.EnqueueManualRun(r.Context(), tx, ws, id, body.rowIDs, dry, autoRunNow())
		return err
	})
	if err != nil {
		h.fail(w, "manual run", err)
		return
	}
	res := wbcrud.NewObj()
	res.Set("job_id", jobID)
	res.Set("fire_key", fireKey)
	res.Set("dry_run", dry)
	httpx.WriteJSON(w, http.StatusOK, res)
}

// runs is GET /triggers/{id}/runs?limit=50 (le 200).
func (h autoRunRoutes) runs(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	ws, id := wc.WorkspaceID, r.PathValue("trigger_id")
	if badPathID(id) {
		httpx.Detail(w, http.StatusNotFound, "trigger not found")
		return
	}
	def, le := int64(50), int64(200)
	limit, ferr := wbcrud.QueryInt(r.URL.Query(), "limit", &def, nil, &le)
	if ferr != nil {
		httpx.ValidationError(w, *ferr)
		return
	}
	if h.disabled(w) {
		return
	}
	runs, err := h.svc.Runs(r.Context(), ws, id, int(limit))
	if err != nil {
		h.fail(w, "run history", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, runs)
}

// runDetail is GET /runs/{run_id}: the run and its action results by id.
func (h autoRunRoutes) runDetail(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	if badPathID(r.PathValue("run_id")) {
		httpx.Detail(w, http.StatusNotFound, "run not found")
		return
	}
	if h.disabled(w) {
		return
	}
	detail, err := h.svc.RunWithResults(r.Context(), wc.WorkspaceID, r.PathValue("run_id"))
	if err != nil {
		h.fail(w, "run detail", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}
