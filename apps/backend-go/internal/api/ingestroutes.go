package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
)

// Inbound rows (routers/ingest.py): per-workbook ingest tokens and POST rows/ingest.
const (
	ingestTokenPrefix    = "wbi_"
	maxIngestRows        = 500 // MAX_INGEST_ROWS: beyond it 413
	ingestIdemRetainDays = 7   // IDEMPOTENCY_RETENTION_DAYS
	ingestIdemKeyMax     = 128
	ingestDisabledDetail = "ingest api disabled"
)

// ingestHookFor builds the row-added hook (a seam for tests).
var ingestHookFor = func(d Deps) automationhook.Hook {
	return d.automationHook()
}

var errIngestIdemConflict = errors.New("ingest: idempotency key recorded concurrently")

// registerIngest registers POST /api/v2/workbooks/{workbook_id}/ingest-token (require_editor) and
// .../rows/ingest (session with tables.write, or a per-workbook ingest token).
func registerIngest(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Identity == nil || !d.workbooksEnabled("ingest") {
		return
	}
	h := ingestRoutes{d: d, hook: ingestHookFor(d), svc: &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}, Now: time.Now}}
	h.session = identity.Authenticate(d.Signer, d.Identity)(identity.Workspace(d.Identity)(http.HandlerFunc(h.sessionIngest)))
	const base = "/api/v2/workbooks/{workbook_id}"
	r.Handle("POST "+base+"/ingest-token", http.HandlerFunc(h.createToken),
		identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin"))
	r.Handle("POST "+base+"/rows/ingest", http.HandlerFunc(h.ingest))
}

type ingestRoutes struct {
	d       Deps
	svc     *wbcrud.Service
	hook    automationhook.Hook
	session http.Handler
}

func (h ingestRoutes) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

func (h ingestRoutes) fail(w http.ResponseWriter, what string, err error) {
	h.log().Error("ingest: "+what+" failed", "err_type", errTypeName(err)) // never the error text: it may echo SQL args
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// ── tokens ───────────────────────────────────────────────────────────────────

func hashIngestToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ingestHashEqual compares two hex digests in constant time (a length mismatch is simply false).
func ingestHashEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// extractIngestToken is _extract_ingest_token: X-Ingest-Token, else a wbi_-prefixed bearer.
func extractIngestToken(xIngest, authorization string) string {
	if raw := strings.TrimSpace(xIngest); raw != "" {
		return raw
	}
	if cand, ok := bearerOf(authorization); ok && strings.HasPrefix(cand, ingestTokenPrefix) {
		return cand
	}
	return ""
}

// bearerOf is `auth.lower().startswith("bearer ")` then `split(" ", 1)[1].strip()`.
func bearerOf(authorization string) (string, bool) {
	if len(authorization) < 7 || !strings.EqualFold(authorization[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(authorization[7:]), true
}

// pyIsoUTC formats an aware UTC datetime like Python's isoformat().
func pyIsoUTC(t time.Time) string {
	u := t.UTC()
	s := u.Format("2006-01-02T15:04:05")
	if us := u.Nanosecond() / 1000; us != 0 {
		s += u.Format(".000000")
	}
	return s + "+00:00"
}

func (h ingestRoutes) createToken(w http.ResponseWriter, r *http.Request) {
	if !h.d.Flags.IngestAPIEnabled {
		httpx.Detail(w, http.StatusNotFound, ingestDisabledDetail)
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		h.fail(w, "token entropy", err)
		return
	}
	raw := ingestTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	prefix := raw[:12]
	wbID := r.PathValue("workbook_id")
	var rotated int64
	var created gen.InsertIngestTokenRow
	err := h.svc.WithTx(r.Context(), wc.WorkspaceID, func(tx pgx.Tx) error {
		// FOR UPDATE serialises concurrent rotations so exactly one token stays live.
		if _, err := h.svc.Owned(r.Context(), tx, wc.WorkspaceID, wbID, true); err != nil {
			return err
		}
		q := gen.New(tx)
		var err error
		if rotated, err = q.RevokeActiveIngestTokens(r.Context(), gen.RevokeActiveIngestTokensParams{WorkbookID: wbID, WorkspaceID: wc.WorkspaceID}); err != nil {
			return err
		}
		created, err = q.InsertIngestToken(r.Context(), gen.InsertIngestTokenParams{
			ID: wbcrud.NewID(), WorkspaceID: wc.WorkspaceID, WorkbookID: wbID, TokenHash: hashIngestToken(raw), Prefix: prefix, CreatedBy: pgtype.Int4{Int32: wc.User.ID, Valid: true}})
		return err
	})
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, ingestTokenResponse{Token: raw, ID: created.ID, Prefix: prefix, WorkbookID: wbID,
			WorkspaceID: wc.WorkspaceID, CreatedAt: pyIsoUTC(created.CreatedAt.Time), Rotated: rotated})
	case errors.Is(err, wbcrud.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, wbcrud.NotFoundDetail)
	default:
		h.fail(w, "create token", err)
	}
}

// ingestTokenResponse is the one-time creation response (the only place the plaintext appears).
type ingestTokenResponse struct {
	Token       string `json:"token"`
	ID          string `json:"id"`
	Prefix      string `json:"prefix"`
	WorkbookID  string `json:"workbook_id"`
	WorkspaceID string `json:"workspace_id"`
	CreatedAt   string `json:"created_at"`
	Rotated     int64  `json:"rotated"`
}

// resolveIngestToken is resolve_ingest_token: the workspace a token may ingest into for workbookID. Failures
// carry the service's strings; the stored hash is re-compared in constant time on top of the indexed lookup.
func (h ingestRoutes) resolveIngestToken(ctx context.Context, raw, workbookID string) (ws string, detail string, err error) {
	presented := hashIngestToken(raw)
	tok, err := gen.New(h.d.Pool).GetIngestTokenByHash(ctx, presented)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "invalid ingest token", nil
	}
	if err != nil {
		return "", "", err
	}
	if !ingestHashEqual(tok.TokenHash, presented) {
		return "", "invalid ingest token", nil
	}
	if tok.RevokedAt.Valid {
		return "", "ingest token revoked", nil
	}
	if tok.WorkbookID != workbookID { // a token is bound to exactly one workbook
		return "", "ingest token not valid for this workbook", nil
	}
	return tok.WorkspaceID, "", nil
}

// ── ingest ───────────────────────────────────────────────────────────────────

// ingest dispatches like Depends(optional_session_workspace): a bearer that is not an ingest token is a
// session (a bad JWT stays 401); otherwise the ingest token path.
func (h ingestRoutes) ingest(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	if b, ok := bearerOf(authz); ok && b != "" && !strings.HasPrefix(b, ingestTokenPrefix) {
		h.session.ServeHTTP(w, r)
		return
	}
	if !h.d.Flags.IngestAPIEnabled {
		httpx.Detail(w, http.StatusNotFound, ingestDisabledDetail)
		return
	}
	raw := extractIngestToken(r.Header.Get("X-Ingest-Token"), authz)
	if raw == "" {
		httpx.Detail(w, http.StatusUnauthorized, "Missing credentials")
		return
	}
	ws, detail, err := h.resolveIngestToken(r.Context(), raw, r.PathValue("workbook_id"))
	if err != nil {
		h.fail(w, "resolve token", err)
		return
	}
	if detail != "" {
		httpx.Detail(w, http.StatusUnauthorized, detail)
		return
	}
	h.withBudget(w, r, ws)
}

func (h ingestRoutes) sessionIngest(w http.ResponseWriter, r *http.Request) {
	if !h.d.Flags.IngestAPIEnabled {
		httpx.Detail(w, http.StatusNotFound, ingestDisabledDetail)
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	ok, err := h.d.Identity.HasPermission(r.Context(), wc.WorkspaceID, wc.User.ID, "tables.write", "admin", "editor")
	if err != nil {
		h.fail(w, "permission check", err)
		return
	}
	if !ok {
		httpx.Detail(w, http.StatusForbidden, "Insufficient workspace role")
		return
	}
	h.withBudget(w, r, wc.WorkspaceID)
}

// withBudget applies plan 04's upload byte budget (503 when full) and body cap before the body is read.
func (h ingestRoutes) withBudget(w http.ResponseWriter, r *http.Request, ws string) {
	withBodyBudget(sharedBudget(h.d.Flags.ImportMemoryBudgetMB), maxRowsBody, func(w http.ResponseWriter, r *http.Request) {
		h.process(w, r, ws)
	})(w, r)
}

func (h ingestRoutes) process(w http.ResponseWriter, r *http.Request, ws string) {
	doc, errs, ok := readRowsDoc(w, r)
	if !ok {
		return
	}
	var req wbcrud.AddRowsRequest
	if errs == nil {
		req, errs = wbcrud.ParseAddRows(doc) // rows min 1, dedupe default true, plan 04 row and key caps
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	if len(req.Rows) > maxIngestRows {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("too many rows: %d > %d per request", len(req.Rows), maxIngestRows))
		return
	}
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if rs := []rune(idem); len(rs) > ingestIdemKeyMax {
		idem = string(rs[:ingestIdemKeyMax])
	}
	wbID := r.PathValue("workbook_id")
	var body []byte
	var replay bool
	err := h.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		var err error
		body, replay, err = h.apply(r.Context(), tx, ws, wbID, req, idem)
		return err
	})
	if errors.Is(err, errIngestIdemConflict) { // a concurrent request recorded this key first: replay its result
		err = h.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
			prior, err := gen.New(tx).GetIngestIdempotency(r.Context(), gen.GetIngestIdempotencyParams{WorkbookID: wbID, IdempotencyKey: idem})
			body, replay = []byte(prior), true
			return err
		})
	}
	switch {
	case err == nil:
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case errors.Is(err, wbcrud.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, wbcrud.NotFoundDetail)
	default:
		h.fail(w, "rows", err)
	}
}

// ingestResult is the response (and the stored idempotency record); field order is Python's dict order.
type ingestResult struct {
	WorkbookID        string   `json:"workbook_id"`
	Added             int      `json:"added"`
	SkippedDuplicates int      `json:"skipped_duplicates"`
	SkippedEmpty      int      `json:"skipped_empty"`
	TotalRows         int      `json:"total_rows"`
	RowIDs            []int64  `json:"row_ids"`
	UnmappedKeys      []string `json:"unmapped_keys"`
}

var (
	leadFieldsOnce sync.Once
	leadFieldNames []string
)

func ingestLeadFields() []string {
	leadFieldsOnce.Do(func() {
		if reg, err := wbregistry.Load(); err == nil {
			leadFieldNames = reg.LeadFields()
		}
	})
	return leadFieldNames
}

// ingestKeyMap is _key_map: lowercase incoming key -> canonical lead field. Every lead field maps to itself;
// each lead_field column also maps its display name.
func ingestKeyMap(cols []wbcrud.Column) map[string]string {
	m := map[string]string{}
	for _, f := range ingestLeadFields() {
		m[strings.ToLower(f)] = f
	}
	for _, c := range cols {
		if t, _ := c.Get("type").(string); t != "lead_field" || !wbcrud.Truthy(c.Get("lead_field")) {
			continue
		}
		lf := wbcrud.PyStr(c.Get("lead_field"))
		if _, ok := m[strings.ToLower(lf)]; !ok {
			m[strings.ToLower(lf)] = lf
		}
		if nv := c.Get("name"); wbcrud.Truthy(nv) {
			if name := wbcrud.TrimPy(wbcrud.PyStr(nv)); name != "" {
				m[strings.ToLower(name)] = lf
			}
		}
	}
	return m
}

// ingestMapRow is _map_row: mapped keys use the canonical field, unknown keys are kept verbatim and recorded.
func ingestMapRow(raw *wbcrud.Obj, keyMap map[string]string, unmapped map[string]struct{}) *wbcrud.Obj {
	out := wbcrud.NewObj()
	for _, k := range raw.Keys() {
		v, _ := raw.Get(k)
		if canon, ok := keyMap[strings.ToLower(wbcrud.TrimPy(k))]; ok {
			out.Set(canon, v)
		} else {
			out.Set(k, v)
			unmapped[k] = struct{}{}
		}
	}
	return out
}

// ingestRowEmpty is `not any(v not in (None, "") for v in row.values())`.
func ingestRowEmpty(row *wbcrud.Obj) bool {
	for _, k := range row.Keys() {
		v, _ := row.Get(k)
		if s, isStr := v.(string); v != nil && !(isStr && s == "") {
			return false
		}
	}
	return true
}

// apply runs inside the caller's workspace transaction: replay lookup, map, dedupe, insert, total, idempotency
// record, row-added hook. It returns the response body and whether it is a stored replay.
func (h ingestRoutes) apply(ctx context.Context, tx pgx.Tx, ws, wbID string, req wbcrud.AddRowsRequest, idem string) ([]byte, bool, error) {
	wb, err := h.svc.Owned(ctx, tx, ws, wbID, true) // FOR UPDATE: positions and totals stay consistent
	if err != nil {
		return nil, false, err
	}
	q := gen.New(tx)
	if idem != "" {
		prior, err := q.GetIngestIdempotency(ctx, gen.GetIngestIdempotencyParams{WorkbookID: wbID, IdempotencyKey: idem})
		if err == nil {
			return []byte(prior), true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, err
		}
	}
	keyMap := ingestKeyMap(wb.Columns)
	unmapped := map[string]struct{}{}
	var mapped []*wbcrud.Obj
	skippedEmpty := 0
	for _, raw := range req.Rows {
		row := ingestMapRow(raw, keyMap, unmapped)
		if ingestRowEmpty(row) {
			skippedEmpty++
			continue
		}
		mapped = append(mapped, row)
	}
	skippedDupes := 0
	if req.Dedupe && len(mapped) > 0 {
		seen, err := wbcrud.ExistingIdentities(ctx, tx, wbID)
		if err != nil {
			return nil, false, err
		}
		kept := mapped[:0:0]
		for _, row := range mapped {
			id := wbcrud.RowIdentity(row)
			if id != "" {
				if _, dup := seen[id]; dup {
					skippedDupes++
					continue
				}
				seen[id] = struct{}{}
			}
			kept = append(kept, row)
		}
		mapped = kept
	}
	ids := []int64{}
	if len(mapped) > 0 {
		maxPos, err := q.MaxWorkbookRowPosition(ctx, wbID)
		if err != nil {
			return nil, false, err
		}
		datas := make([]string, len(mapped))
		leadIDs := make([]string, len(mapped)) // external rows carry no leads-DB identity
		for i, row := range mapped {
			datas[i] = wbcrud.PyDumps(row, false)
		}
		got, err := q.AppendWorkbookRows(ctx, gen.AppendWorkbookRowsParams{WorkspaceID: ws, WorkbookID: wbID, StartPosition: maxPos, Datas: datas, LeadIds: leadIDs})
		if err != nil {
			return nil, false, err
		}
		if len(got) != len(mapped) {
			return nil, false, wbcrud.ErrNotFound
		}
		for _, id := range got {
			ids = append(ids, int64(id))
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	total, err := q.CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: wbID, WorkspaceID: ws})
	if err != nil {
		return nil, false, err
	}
	if int(total) != wb.TotalRows { // SQLAlchemy skips an UPDATE that changes nothing
		if err := q.SetWorkbookTotalRows(ctx, gen.SetWorkbookTotalRowsParams{TotalRows: int32(total), WorkbookID: wbID, WorkspaceID: ws}); err != nil {
			return nil, false, err
		}
	}
	keys := make([]string, 0, len(unmapped))
	for k := range unmapped {
		keys = append(keys, k)
	}
	sort.Strings(keys) // UTF-8 byte order is code-point order, like Python's sorted(str)
	res := ingestResult{WorkbookID: wbID, Added: len(ids), SkippedDuplicates: skippedDupes, SkippedEmpty: skippedEmpty,
		TotalRows: int(total), RowIDs: ids, UnmappedKeys: keys}
	body, err := wbcrud.MarshalNoEscape(res)
	if err != nil {
		return nil, false, err
	}
	body = []byte(strings.TrimRight(string(body), "\n"))
	if idem != "" {
		n, err := q.InsertIngestIdempotency(ctx, gen.InsertIngestIdempotencyParams{ID: wbcrud.NewID(), WorkspaceID: ws, WorkbookID: wbID,
			IdempotencyKey: idem, Response: string(body)})
		if err != nil {
			return nil, false, err
		}
		if n == 0 {
			return nil, false, errIngestIdemConflict // roll back our rows; the winner's record is replayed
		}
		if err := q.PruneIngestIdempotency(ctx, gen.PruneIngestIdempotencyParams{WorkbookID: wbID, RetentionDays: ingestIdemRetainDays}); err != nil {
			return nil, false, err
		}
	}
	if len(ids) > 0 && h.hook != nil {
		_, _ = h.hook.RowsAdded(ctx, tx, ws, wbID, ids) // inside the caller's tx; errors logged and swallowed by the hook
	}
	return body, false, nil
}
