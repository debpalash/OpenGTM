package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

const cellRunMaxBody = 1 << 20 // RunCellRequest is one bool: far more than enough

// newCellRunner builds the provider runner of the cell run route (replaced by tests).
// Credentials resolve per workspace through the settings resolver (workspace, then operator,
// then env); without a secrets envelope no credential exists, so keyed providers are skipped
// as "not configured" before any spend is reserved.
var newCellRunner = func(d Deps) workbook.ProviderRunner {
	var sec providerrun.Secrets = noSecrets{}
	if d.Envelope != nil && d.Pool != nil {
		sec = secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), d.getenv())
	}
	var allow []string
	for _, h := range strings.Split(d.getenv()("EGRESS_ALLOW_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			allow = append(allow, h)
		}
	}
	// Declarative providers (connector manifests) use the app env's egress client and URL guard, as in the worker.
	appEnv := d.Config.AppEnv
	if appEnv == "" {
		appEnv = d.getenv()("APP_ENV")
	}
	return providerrun.New(providerrun.Deps{Pool: d.Pool, Secrets: sec, OperatorAllow: allow, Log: d.Log,
		ManifestHTTP: egress.NewClientForEnv(appEnv, allow),
		URLCheck:     func(raw string) error { return egress.CheckURLFor(appEnv, allow, raw, true) }})
}

type noSecrets struct{}

func (noSecrets) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

// cellRoutes serves POST /api/workbooks/{workbook_id}/rows/{row_id}/cells/{col_id}/run.
type cellRoutes struct {
	d    Deps
	deps workbook.Deps
	log  *slog.Logger
}

// registerWorkbookCell registers the single-cell run route (plan 06 Task 25): an editor
// route limited to 120/min that runs one cell in the request and answers the fresh cell.
// Every column type runs in this process (plan 15: the Python API proxy is gone).
func registerWorkbookCell(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	h := &cellRoutes{d: d, log: log, deps: cellDeps(d, log)}
	// slowapi wraps the endpoint, so authentication, role and path validation answer first
	// and only a request that reaches the endpoint spends the 120/min budget.
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin"), h.validatePath}
	if d.Limiter != nil {
		mws = append(mws, d.Limiter.Limit("120/minute"))
	}
	r.Handle("POST /api/workbooks/{workbook_id}/rows/{row_id}/cells/{col_id}/run", http.HandlerFunc(h.run), mws...)
}

// validatePath is FastAPI's `row_id: int` path validation.
func (h *cellRoutes) validatePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.PathValue("row_id")
		if n, ok := wbcrud.ParseIntBig(raw); !ok || !n.IsInt64() {
			httpx.ValidationError(w, httpx.FieldError{Type: "int_parsing", Loc: []any{"path", "row_id"}, Input: raw,
				Msg: "Input should be a valid integer, unable to parse string as an integer"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *cellRoutes) run(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cellRunMaxBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	force, errs := parseRunCellBody(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	rowID, _ := wbcrud.ParseIntBig(r.PathValue("row_id"))
	wc, _ := identity.WorkspaceFrom(r.Context())
	res, err := workbook.EnrichCell(r.Context(), h.deps, workbook.CellRequest{
		WorkspaceID: wc.WorkspaceID, WorkbookID: r.PathValue("workbook_id"), RowID: rowID.Int64(),
		ColumnID: r.PathValue("col_id"), Force: force})
	switch {
	case errors.Is(err, workbook.ErrClientGone), err == nil && r.Context().Err() != nil:
		// The client left. Before dispatch nothing ran; after it the cell was still persisted
		// (the run is detached), only this answer is dropped.
		httpx.Detail(w, 499, "Client Closed Request")
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, res)
	case errors.Is(err, workbook.ErrWorkbookNotFound):
		httpx.Detail(w, http.StatusNotFound, "Workbook not found")
	case errors.Is(err, workbook.ErrColumnNotFound):
		httpx.Detail(w, http.StatusNotFound, "Enrichment column not found")
	case errors.Is(err, workbook.ErrRowNotFound):
		httpx.Detail(w, http.StatusNotFound, "Workbook row not found")
	case errors.Is(err, workbook.ErrLegacyRowNotFound):
		httpx.Detail(w, http.StatusNotFound, "Row not found")
	case r.Context().Err() != nil:
		// The client went away: the run was cancelled (a paid call in flight stays
		// uncertain). Nobody reads this answer; 499 keeps the access log honest.
		httpx.Detail(w, 499, "Client Closed Request")
	default:
		h.log.Error("cell run failed", "workbook", r.PathValue("workbook_id"), "err_type", fmt.Sprintf("%T", err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
}

// parseRunCellBody validates `body: RunCellRequest = None` (force: bool = False). No body is the
// default request; unknown keys are ignored like pydantic does.
func parseRunCellBody(raw []byte) (force bool, errs []httpx.FieldError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false, nil
	}
	doc, errs := wbcrud.DecodeBody(raw)
	if errs != nil {
		return false, errs
	}
	obj, ok := doc.(*wbcrud.Obj)
	if !ok {
		return false, []httpx.FieldError{{Type: "model_attributes_type", Loc: []any{"body"}, Input: doc,
			Msg: "Input should be a valid dictionary or object to extract fields from"}}
	}
	v, present := obj.Get("force")
	if !present {
		return false, nil
	}
	return lenientBool(v, []any{"body", "force"})
}

// lenientBool is pydantic's lax bool validation (bool, 0/1, "true"/"no"/...).
func lenientBool(v any, loc []any) (bool, []httpx.FieldError) {
	bad := func(typ, msg string) (bool, []httpx.FieldError) {
		return false, []httpx.FieldError{{Type: typ, Loc: loc, Msg: msg, Input: v}}
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case json.Number:
		switch t.String() {
		case "0", "0.0":
			return false, nil
		case "1", "1.0":
			return true, nil
		}
		return bad("bool_parsing", "Input should be a valid boolean, unable to interpret input")
	case string:
		switch strings.ToLower(t) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return bad("bool_parsing", "Input should be a valid boolean, unable to interpret input")
	}
	return bad("bool_type", "Input should be a valid boolean")
}

// cellDeps is the executor wiring of the single-cell route. With GO_EXECUTOR_FULL_RUN
// (plan 15; permanently on since the cutover) output columns and email verification run in
// process: output clients and the verify cascade resolve workspace credentials, the webhook
// goes through the SSRF-guarded egress client.
func cellDeps(d Deps, log *slog.Logger) workbook.Deps {
	if log == nil {
		log = slog.Default()
	}
	deps := workbook.Deps{Pool: d.Pool, Runner: newCellRunner(d), Log: log}
	deps.AICols = aiColumns(d, log)
	wx := config.LoadWbExec(d.getenv())
	for _, p := range wx.Problems {
		log.Error("cell run: " + p)
	}
	if !wx.FullRun {
		return deps
	}
	lu := &leadUtils{d: d}
	allow := lu.allowHosts()
	deps.FullRun, deps.Provenance = true, wx.ProvenanceTracking
	deps.Client = egress.NewClientForEnv(d.Config.AppEnv, allow)
	deps.URLCheck = func(raw string) error { return egress.CheckURLFor(d.Config.AppEnv, allow, raw, true) }
	deps.Output = workbook.NewLiveOutputPorts(d.Pool, lu.resolver(), deps.Client, log)
	deps.Verify = workbook.VerifyFunc(func(ctx context.Context, ws, email string) (emailverify.Result, error) {
		return emailverify.Cascade(ctx, lu.verifyDeps(), ws, email)
	})
	return deps
}
