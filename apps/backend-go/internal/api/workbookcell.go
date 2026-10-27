package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

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

const (
	cellRunMaxBody    = 1 << 20 // RunCellRequest is one bool: far more than enough
	cellProxyMaxReply = 8 << 20
	cellProxyTimeout  = 5 * time.Minute
)

// newCellRunner builds the provider runner of the cell run route (replaced by tests).
// Credentials resolve per workspace through the settings resolver (workspace, then operator,
// then env); without a secrets envelope no credential exists, so keyed providers are skipped
// as "not configured" before any spend is reserved.
var newCellRunner = func(d Deps) workbook.ProviderRunner {
	var sec providerrun.Secrets = noSecrets{}
	if d.Envelope != nil && d.Pool != nil {
		sec = secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), d.getenv())
	}
	var py providerrun.PyCaller
	if d.PyWorker != nil { // never wrap a nil *Client in a non-nil interface
		py = d.PyWorker
	}
	var allow []string
	for _, h := range strings.Split(d.getenv()("EGRESS_ALLOW_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			allow = append(allow, h)
		}
	}
	return providerrun.New(providerrun.Deps{Pool: d.Pool, Secrets: sec, Py: py, OperatorAllow: allow, Log: d.Log})
}

type noSecrets struct{}

func (noSecrets) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

// cellRoutes serves POST /api/workbooks/{workbook_id}/rows/{row_id}/cells/{col_id}/run.
type cellRoutes struct {
	d    Deps
	py   string // validated PYTHON_API_URL (empty = proxy unavailable)
	deps workbook.Deps
	log  *slog.Logger
	http *http.Client
}

// registerWorkbookCell registers the single-cell run route (plan 06 Task 25): an editor
// route limited to 120/min that runs one cell in the request and answers the fresh cell.
// Output columns (and email verification, which needs the Python cascade) are proxied to the
// Python API unchanged; see workbook.ErrNeedsPython.
func registerWorkbookCell(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	h := &cellRoutes{d: d, log: log, deps: cellDeps(d, log),
		http: &http.Client{Timeout: cellProxyTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	h.py = validPythonURL(d.getenv()("PYTHON_API_URL"), log)
	// slowapi wraps the endpoint, so authentication, role and path validation answer first
	// and only a request that reaches the endpoint spends the 120/min budget.
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin"), h.validatePath}
	if d.Limiter != nil {
		mws = append(mws, d.Limiter.Limit("120/minute"))
	}
	r.Handle("POST /api/workbooks/{workbook_id}/rows/{row_id}/cells/{col_id}/run", http.HandlerFunc(h.run), mws...)
}

// validPythonURL checks PYTHON_API_URL once: an absolute http(s) URL with a host and no
// credentials. Anything else is logged and treated as unset (the proxy answers 502).
func validPythonURL(raw string, log *slog.Logger) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		log.Error("PYTHON_API_URL is not an http(s) URL with a host; the cell run proxy is disabled")
		return ""
	}
	return strings.TrimRight(raw, "/")
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
	case errors.Is(err, workbook.ErrNeedsPython):
		h.proxy(w, r, raw, workbook.NeedsPythonReason(err))
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

// hopByHop headers are never forwarded in either direction.
var hopByHop = map[string]bool{"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Host": true, "Content-Length": true}

// spoofable headers state a client address: never taken from the caller.
var spoofable = map[string]bool{"X-Forwarded-For": true, "X-Real-Ip": true, "Forwarded": true, "X-Forwarded-Host": true,
	"X-Forwarded-Proto": true}

// proxy sends the request to the Python API and relays its answer (status, headers, body)
// unchanged. nginx cannot route on the request body or the column type, so output columns
// reach this process and are forwarded; PYTHON_API_URL names the FastAPI base URL.
func (h *cellRoutes) proxy(w http.ResponseWriter, r *http.Request, body []byte, reason string) {
	base := h.py
	if base == "" {
		h.log.Error("cell run needs the python api but PYTHON_API_URL is not set", "reason", reason)
		httpx.Detail(w, http.StatusBadGateway, "Python API is not configured")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		httpx.Detail(w, http.StatusBadGateway, "Python API unavailable")
		return
	}
	for k, vs := range r.Header {
		if hopByHop[http.CanonicalHeaderKey(k)] || spoofable[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// Python's per-IP limiter keys on the forwarded client address: state the real peer.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		req.Header.Set("X-Forwarded-For", host)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			httpx.Detail(w, 499, "Client Closed Request")
			return
		}
		h.log.Error("python api proxy failed", "reason", reason, "err_type", fmt.Sprintf("%T", err))
		httpx.Detail(w, http.StatusBadGateway, "Python API unavailable")
		return
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, cellProxyMaxReply+1))
	if err != nil || len(reply) > cellProxyMaxReply {
		// Never relay a truncated body after a status line was sent.
		h.log.Error("python api reply unreadable or too large", "reason", reason, "bytes", len(reply))
		httpx.Detail(w, http.StatusBadGateway, "Python API unavailable")
		return
	}
	for k, vs := range resp.Header {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(reply)
}

// cellDeps is the executor wiring of the single-cell route. With GO_EXECUTOR_FULL_RUN
// (plan 15) output columns and email verification run in process (the Python API proxy is
// never used): output clients and the verify cascade resolve workspace credentials, the
// webhook goes through the SSRF-guarded egress client.
func cellDeps(d Deps, log *slog.Logger) workbook.Deps {
	if log == nil {
		log = slog.Default()
	}
	deps := workbook.Deps{Pool: d.Pool, Runner: newCellRunner(d), Log: log}
	if d.PyWorker != nil {
		deps.Py = d.PyWorker
	}
	wx, err := config.LoadWbExec(d.getenv())
	if err != nil {
		log.Error("cell run: GO_EXECUTOR_FULL_RUN / PROVENANCE_TRACKING_ENABLED unreadable; full run stays off", "err", err)
		return deps
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
