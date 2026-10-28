package api

import (
	"encoding/json"
	"errors"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/records"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"
	"time"
)

func RegisterRecords(r *httpx.Router, d Deps) {
	h := recordRoutes{records.Store{Pool: d.Pool, Hook: d.automationHook()}, d}
	read := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	write := append(append([]httpx.Middleware{}, read...), identity.RequireRole("tables.write", "editor", "admin"))
	r.Handle("GET /api/records/{kind}", http.HandlerFunc(h.list), read...)
	r.Handle("GET /api/records/{kind}/export", http.HandlerFunc(h.export), read...)
	r.Handle("GET /api/records/{kind}/workflow", http.HandlerFunc(h.workflow), read...)
	r.Handle("PUT /api/records/{kind}/workflow", http.HandlerFunc(h.saveWorkflow), write...)
	r.Handle("POST /api/records/company/find-people", http.HandlerFunc(h.findPeople), write...)
	r.Handle("POST /api/records/person/enroll", http.HandlerFunc(h.enrollPeople), write...)
	r.Handle("POST /api/records/{kind}/audience", http.HandlerFunc(h.audience), write...)
	r.Handle("POST /api/records/{kind}/workbook", http.HandlerFunc(h.workbook), write...)
	r.Handle("GET /api/records/{kind}/{id}/history", http.HandlerFunc(h.history), read...)
	r.Handle("GET /api/records/{kind}/{id}", http.HandlerFunc(h.get), read...)
	r.Handle("POST /api/records/{kind}", http.HandlerFunc(h.create), write...)
	r.Handle("PATCH /api/records/{kind}/{id}", http.HandlerFunc(h.patch), write...)
	r.Handle("POST /api/record-imports/preview", http.HandlerFunc(h.preview), write...)
	r.Handle("POST /api/record-imports", http.HandlerFunc(h.importRecords), write...)
	r.Handle("GET /api/record-snapshots", http.HandlerFunc(h.snapshot), read...)
	r.Handle("POST /api/record-snapshots/preview", http.HandlerFunc(h.restorePreview), write...)
	r.Handle("POST /api/record-snapshots/restore", http.HandlerFunc(h.restore), write...)
}

func (h recordRoutes) snapshot(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	out, e := h.s.Snapshot(r.Context(), wc.WorkspaceID)
	if e != nil {
		h.fail(w, e)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="opengtm-records.json"`)
	httpx.WriteJSON(w, 200, out)
}
func (h recordRoutes) restorePreview(w http.ResponseWriter, r *http.Request) {
	h.restoreRun(w, r, true)
}
func (h recordRoutes) restore(w http.ResponseWriter, r *http.Request) { h.restoreRun(w, r, false) }
func (h recordRoutes) restoreRun(w http.ResponseWriter, r *http.Request, preview bool) {
	var b records.RestoreRequest
	if !recordBodyLimit(w, r, &b, 128<<20) {
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	out, e := h.s.Restore(r.Context(), wc.WorkspaceID, b, preview)
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 200, out)
}

type recordRoutes struct {
	s records.Store
	d Deps
}

func recordScope(w http.ResponseWriter, r *http.Request) (string, records.Kind, bool) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	k := records.Kind(r.PathValue("kind"))
	if !k.Valid() {
		httpx.Detail(w, 422, "record type must be company or person")
		return "", k, false
	}
	return wc.WorkspaceID, k, true
}
func recordBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return recordBodyLimit(w, r, v, 32<<20)
}
func recordBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		httpx.Detail(w, 422, "Invalid record request: "+e.Error())
		return false
	}
	return true
}
func (h recordRoutes) fail(w http.ResponseWriter, e error) {
	status := 422
	var pg *pgconn.PgError
	if errors.Is(e, records.ErrNotFound) {
		status = 404
	} else if errors.Is(e, records.ErrConflict) {
		status = 409
	} else if errors.As(e, &pg) {
		switch pg.Code {
		case "23505", "40001":
			status = 409
		case "P0002":
			status = 404
		case "42501":
			status = 403
		case "22023":
			status = 422
		default:
			status = 500
		}
	}
	if status == 500 {
		if h.d.Log != nil {
			h.d.Log.Error("record operation failed", "err", e)
		}
		httpx.Detail(w, status, "Record operation failed")
		return
	}
	httpx.Detail(w, status, records.PublicError(e))
}
func (h recordRoutes) list(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	limit, ok := queryInt(w, r, "limit", 100, intp(1), intp(500))
	if !ok {
		return
	}
	offset, ok := queryInt(w, r, "offset", 0, intp(0), nil)
	if !ok {
		return
	}
	out, e := h.s.List(r.Context(), ws, k, r.URL.Query().Get("search"), limit, offset)
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 200, out)
}
func (h recordRoutes) get(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	out, e := h.s.Get(r.Context(), ws, k, r.PathValue("id"))
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 200, out)
}
func (h recordRoutes) create(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	var b records.Write
	if !recordBody(w, r, &b) {
		return
	}
	out, e := h.s.Upsert(r.Context(), ws, k, b, "manual", "")
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 201, out)
}
func (h recordRoutes) patch(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	var b struct {
		Version int64 `json:"version"`
		records.Write
	}
	if !recordBody(w, r, &b) {
		return
	}
	out, e := h.s.Patch(r.Context(), ws, k, r.PathValue("id"), b.Version, b.Write)
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 200, out)
}
func (h recordRoutes) preview(w http.ResponseWriter, r *http.Request)       { h.importRun(w, r, true) }
func (h recordRoutes) importRecords(w http.ResponseWriter, r *http.Request) { h.importRun(w, r, false) }
func (h recordRoutes) importRun(w http.ResponseWriter, r *http.Request, preview bool) {
	var b records.ImportRequest
	if !recordBody(w, r, &b) {
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	out, e := h.s.Import(r.Context(), wc.WorkspaceID, b, preview)
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 200, out)
}
func (h recordRoutes) export(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="opengtm-`+string(k)+`.csv"`)
	if e := h.s.ExportCSV(r.Context(), ws, k, w); e != nil && h.d.Log != nil {
		h.d.Log.Error("record export failed", "err", e)
	}
}
func (h recordRoutes) workflow(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	out, e := h.s.Workflow(r.Context(), ws, k)
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 200, out)
}
func (h recordRoutes) saveWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	var b records.Workflow
	if !recordBody(w, r, &b) {
		return
	}
	if e := h.s.SaveWorkflow(r.Context(), ws, k, b); e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 200, b)
}
func (h recordRoutes) workbook(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	var b struct {
		IDs  []string `json:"ids"`
		Name string   `json:"name"`
	}
	if !recordBody(w, r, &b) {
		return
	}
	id, e := h.s.Workbook(r.Context(), ws, k, b.IDs, b.Name)
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 201, map[string]any{"workbook_id": id})
}

func (h recordRoutes) audience(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	var b struct {
		IDs  []string `json:"ids"`
		Name string   `json:"name"`
	}
	if !recordBody(w, r, &b) {
		return
	}
	if len(b.IDs) == 0 || len(b.IDs) > 10000 || b.Name == "" || len(b.Name) > 200 {
		httpx.Detail(w, 422, "provide a name and 1 to 10000 record IDs")
		return
	}
	id := wbcrud.NewID()
	filter, _ := json.Marshal(map[string]any{"_record_type": k, "record_ids": b.IDs})
	e := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		table := "company_entities"
		if k == records.Person {
			table = "person_entities"
		}
		var n int
		if e := tx.QueryRow(r.Context(), `SELECT count(DISTINCT id) FROM `+table+` WHERE workspace_id=$1 AND id=ANY($2::text[])`, ws, b.IDs).Scan(&n); e != nil {
			return e
		}
		unique := map[string]bool{}
		for _, v := range b.IDs {
			unique[v] = true
		}
		if n != len(unique) {
			return records.ErrNotFound
		}
		_, e := tx.Exec(r.Context(), `INSERT INTO audiences(id,workspace_id,name,filters,record_type) VALUES($1,$2,$3,$4,$5)`, id, ws, b.Name, filter, k)
		return e
	})
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 201, map[string]any{"audience_id": id})
}

func (h recordRoutes) enrollPeople(w http.ResponseWriter, r *http.Request) {
	var b struct {
		IDs           []string `json:"ids"`
		SequenceID    string   `json:"sequence_id"`
		ConsentSource string   `json:"consent_source"`
	}
	if !recordBody(w, r, &b) {
		return
	}
	if len(b.IDs) == 0 || len(b.IDs) > 5000 || b.SequenceID == "" || b.ConsentSource == "" || len(b.ConsentSource) > 120 {
		httpx.Detail(w, 422, "select 1 to 5000 people, a sequence and a consent source")
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	enrolled := 0
	skipped := []map[string]string{}
	e := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) error {
		if ok, e := outreachstore.SequenceExists(r.Context(), tx, wc.WorkspaceID, b.SequenceID); e != nil {
			return e
		} else if !ok {
			return records.ErrNotFound
		}
		for _, pid := range b.IDs {
			_, created, e := outreachstore.EnrollPerson(r.Context(), tx, wc.WorkspaceID, b.SequenceID, pid, b.ConsentSource, time.Now().UTC())
			if errors.Is(e, outreachstore.ErrNoEmail) || errors.Is(e, outreachstore.ErrPersonSuppressed) {
				skipped = append(skipped, map[string]string{"person_id": pid, "reason": e.Error()})
				continue
			}
			if e != nil {
				return e
			}
			if created {
				enrolled++
			} else {
				skipped = append(skipped, map[string]string{"person_id": pid, "reason": "already_enrolled"})
			}
		}
		return nil
	})
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 200, map[string]any{"enrolled": enrolled, "skipped": skipped})
}

func (h recordRoutes) history(w http.ResponseWriter, r *http.Request) {
	ws, k, ok := recordScope(w, r)
	if !ok {
		return
	}
	before, ok := queryInt(w, r, "before", 0, intp(0), nil)
	if !ok {
		return
	}
	out, e := h.s.History(r.Context(), ws, k, r.PathValue("id"), int64(before))
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 200, out)
}

func (h recordRoutes) findPeople(w http.ResponseWriter, r *http.Request) {
	var b records.Discovery
	if !recordBody(w, r, &b) {
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	id, e := h.s.Discover(r.Context(), wc.WorkspaceID, b)
	if e != nil {
		h.fail(w, e)
		return
	}
	httpx.WriteJSON(w, 201, map[string]any{"workbook_id": id})
}
