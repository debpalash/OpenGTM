package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/personintel"
)

const (
	maxPersonBody = 1 << 20
	// personEnrichTimeout bounds the whole enrichment (Python has no bound; the WebSocket route uses 120 s too).
	personEnrichTimeout = 120 * time.Second
	personInvalidURL    = "Invalid LinkedIn URL. Expected format: https://www.linkedin.com/in/username"
)

// personRoutes serves /api/person (routers/person_intel.py). person_intel is a global table (no workspace
// column, no RLS) and every route needs a global admin, so there is no workspace transaction.
type personRoutes struct {
	d      Deps
	egress *http.Client
	// gate is the WS handler: REST enrichment shares its stream pool and its one-run-per-admin map, so
	// the two entry points cannot add up to unbounded concurrency.
	gate *personWS
	// enrich is a seam for tests; production runs personintel.Enrich.
	enrich func(ctx context.Context, d personintel.Deps, linkedinURL string, deep bool) (personintel.Profile, error)
}

func newPersonRoutes(d Deps) *personRoutes {
	cl := d.Egress
	if cl == nil {
		cl = egress.NewClientForEnv(d.Config.AppEnv, d.EgressAllowHosts)
	}
	return &personRoutes{d: d, egress: cl, gate: newPersonWS(d),
		enrich: func(ctx context.Context, pd personintel.Deps, u string, deep bool) (personintel.Profile, error) {
			return personintel.Enrich(ctx, pd, u, deep, nil)
		}}
}

// registerPersonIntelRoutes: enrich, history, get and delete by record_id. The literal /api/person/history is
// registered beside /api/person/{record_id}; ServeMux precedence makes the literal win like FastAPI's
// declaration order.
func registerPersonIntelRoutes(r *httpx.Router, d Deps) { newPersonRoutes(d).register(r) }

// registerPersonIntelShared mounts the REST routes and the WS on one gate (one pool, one run per admin).
func registerPersonIntelShared(r *httpx.Router, d Deps) {
	ws := newPersonWS(d)
	p := newPersonRoutes(d)
	p.gate = ws
	p.register(r)
	ws.register(r)
}

func (p *personRoutes) register(r *httpx.Router) {
	admin := []httpx.Middleware{identity.Authenticate(p.d.Signer, p.d.Identity), identity.RequireGlobalAdmin()}
	r.Handle("POST /api/person/enrich", http.HandlerFunc(p.enrichRoute), admin...)
	r.Handle("GET /api/person/history", http.HandlerFunc(p.history), admin...)
	r.Handle("GET /api/person/{record_id}", http.HandlerFunc(p.detail), admin...)
	r.Handle("DELETE /api/person/{record_id}", http.HandlerFunc(p.remove), admin...)
}

func (p *personRoutes) fail(w http.ResponseWriter, what string, err error) {
	log := p.d.Log
	if log == nil {
		log = slog.Default()
	}
	log.Error("person intel: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// inTx runs fn in one transaction on the app pool and commits it.
func (p *personRoutes) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// enrichBody validates EnrichRequest {linkedin_url: str, deep: bool = False} the way pydantic does.
func enrichBody(w http.ResponseWriter, r *http.Request) (url string, deep bool, ok bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPersonBody))
	var body any
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if err = dec.Decode(&body); err == nil && dec.More() {
			err = io.ErrUnexpectedEOF
		}
	}
	if err != nil {
		httpx.ValidationError(w, httpx.FieldError{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}})
		return "", false, false
	}
	obj, isObj := body.(map[string]any)
	if !isObj {
		httpx.ValidationError(w, httpx.FieldError{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body})
		return "", false, false
	}
	var errs []httpx.FieldError
	switch v := obj["linkedin_url"].(type) {
	case string:
		url = v
	default:
		if _, present := obj["linkedin_url"]; !present {
			errs = append(errs, httpx.FieldError{Type: "missing", Loc: []any{"body", "linkedin_url"}, Msg: "Field required", Input: obj})
		} else {
			errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", "linkedin_url"}, Msg: "Input should be a valid string", Input: v})
		}
	}
	if dv, present := obj["deep"]; present {
		switch v := dv.(type) {
		case bool:
			deep = v
		case nil:
			errs = append(errs, httpx.FieldError{Type: "bool_type", Loc: []any{"body", "deep"}, Msg: "Input should be a valid boolean", Input: nil})
		case string:
			b, good := parseBoolQuery(strings.TrimSpace(v))
			if !good {
				errs = append(errs, httpx.FieldError{Type: "bool_parsing", Loc: []any{"body", "deep"},
					Msg: "Input should be a valid boolean, unable to interpret input", Input: v})
			}
			deep = b
		case float64:
			if v != 0 && v != 1 {
				errs = append(errs, httpx.FieldError{Type: "bool_parsing", Loc: []any{"body", "deep"},
					Msg: "Input should be a valid boolean, unable to interpret input", Input: v})
			}
			deep = v == 1
		default:
			errs = append(errs, httpx.FieldError{Type: "bool_type", Loc: []any{"body", "deep"}, Msg: "Input should be a valid boolean", Input: v})
		}
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return "", false, false
	}
	return url, deep, true
}

func (p *personRoutes) enrichRoute(w http.ResponseWriter, r *http.Request) {
	url, deep, ok := enrichBody(w, r)
	if !ok {
		return
	}
	if !personintel.ValidLinkedInURL(url) {
		httpx.Detail(w, http.StatusBadRequest, personInvalidURL)
		return
	}
	pr, hasPr := identity.PrincipalFrom(r.Context())
	if hasPr { // Go-only bound (Python has none): one run per admin, shared with the WS, and a shared-pool slot
		if !p.gate.startRun(pr.User.ID) {
			httpx.Detail(w, http.StatusTooManyRequests, "Enrichment already running")
			return
		}
		defer p.gate.endRun(pr.User.ID)
	}
	select {
	case p.gate.sem <- struct{}{}:
		defer func() { <-p.gate.sem }()
	default:
		httpx.Detail(w, http.StatusServiceUnavailable, "Too many concurrent streams")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), personEnrichTimeout)
	defer cancel()
	deps := personintel.Deps{HTTP: p.egress}
	if hasPr {
		deps.WorkspaceID = PersonIntelWorkspaceID(ctx, p.d, pr.User.ID, r.Header.Get("X-Workspace-Id"))
	}
	if p.d.PyWorker != nil {
		deps.PyWorker = p.d.PyWorker
	}
	// A pipeline failure is not an error: like Python the profile comes back with status "failed" (including an
	// unparseable username, whose ValueError the Python service swallows) and is saved and returned as usual.
	profile, err := p.enrich(ctx, deps, url, deep)
	if err != nil {
		p.enrichFailed(w, err)
		return
	}
	// The save has its own budget (the WS gives it 30 s too): an enrichment that finishes near the 120 s
	// deadline must not fail its save after the work is done.
	sctx, scancel := context.WithTimeout(context.WithoutCancel(r.Context()), personWSSaveTimeout)
	defer scancel()
	var id int64
	if err := p.inTx(sctx, func(tx pgx.Tx) (e error) { id, e = personintel.Save(sctx, tx, profile); return }); err != nil {
		p.enrichFailed(w, err)
		return
	}
	pj, err := profile.JSON()
	if err != nil {
		p.enrichFailed(w, err)
		return
	}
	writeRaw(w, http.StatusOK, append(append([]byte(`{"id":`+strconv.FormatInt(id, 10)+`,"status":"success","profile":`), pj...), '}'))
}

// enrichFailed answers 500 "Enrichment failed". Python echoes str(e), which can carry connection strings and
// internal hosts; Go logs only the error type.
func (p *personRoutes) enrichFailed(w http.ResponseWriter, err error) {
	log := p.d.Log
	if log == nil {
		log = slog.Default()
	}
	log.Error("person intel: enrichment failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Enrichment failed")
}

func (p *personRoutes) history(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	skip := v.integer("skip", 0, i64(0), nil)
	limit := v.integer("limit", 30, i64(0), nil)
	if v.fail(w) {
		return
	}
	var rows []personintel.HistoryRow
	if err := p.inTx(r.Context(), func(tx pgx.Tx) (e error) {
		rows, e = personintel.History(r.Context(), tx, int(skip), int(limit))
		return
	}); err != nil {
		p.fail(w, "history", err)
		return
	}
	out := []byte{'['}
	for i, row := range rows {
		b, err := row.JSON()
		if err != nil {
			p.fail(w, "history", err)
			return
		}
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, b...)
	}
	writeRaw(w, http.StatusOK, append(out, ']'))
}

// recordID parses {record_id} like pydantic's lax int; an id outside int32 cannot exist (-1: 404).
func recordID(r *http.Request) (int64, *httpx.FieldError) {
	raw := r.PathValue("record_id")
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return -1, nil
		}
		return 0, &httpx.FieldError{Type: "int_parsing", Loc: []any{"path", "record_id"},
			Msg: "Input should be a valid integer, unable to parse string as an integer", Input: raw}
	}
	return n, nil
}

func (p *personRoutes) detail(w http.ResponseWriter, r *http.Request) {
	id, perr := recordID(r)
	if perr != nil {
		httpx.ValidationError(w, *perr)
		return
	}
	var d *personintel.Detail
	if err := p.inTx(r.Context(), func(tx pgx.Tx) (e error) { d, e = personintel.Get(r.Context(), tx, id); return }); err != nil {
		p.fail(w, "detail", err)
		return
	}
	if d == nil {
		httpx.Detail(w, http.StatusNotFound, "Record not found")
		return
	}
	b, err := d.JSON()
	if err != nil {
		p.fail(w, "detail", err)
		return
	}
	writeRaw(w, http.StatusOK, b)
}

func (p *personRoutes) remove(w http.ResponseWriter, r *http.Request) {
	id, perr := recordID(r)
	if perr != nil {
		httpx.ValidationError(w, *perr)
		return
	}
	var existed bool
	if err := p.inTx(r.Context(), func(tx pgx.Tx) (e error) { existed, e = personintel.Delete(r.Context(), tx, id); return }); err != nil {
		p.fail(w, "delete", err)
		return
	}
	if !existed {
		httpx.Detail(w, http.StatusNotFound, "Record not found")
		return
	}
	writeRaw(w, http.StatusOK, []byte(`{"status":"deleted","id":`+strconv.FormatInt(id, 10)+`}`))
}

// PersonIntelWorkspaceID is the workspace_id a person-intel deep scrape sends to the Python worker, which rejects
// an empty one. The routes are global-admin only and not workspace-scoped, so the gate is not applied: it is the
// X-Workspace-Id header when the user belongs to that workspace, else the user's active or first workspace, else
// personintel.DefaultWorkspaceID. A workspace the caller is not a member of is never forwarded. The REST route
// and the WebSocket route (read the header from the upgrade request) use this one function.
func PersonIntelWorkspaceID(ctx context.Context, d Deps, userID int32, header string) string {
	if d.Identity == nil {
		return personintel.DefaultWorkspaceID
	}
	if header != "" {
		if ok, err := d.Identity.IsMember(ctx, header, userID); err == nil && ok {
			return header
		}
	}
	if ws, err := d.Identity.ActiveWorkspaceID(ctx, userID); err == nil && ws != "" {
		return ws
	}
	return personintel.DefaultWorkspaceID
}
