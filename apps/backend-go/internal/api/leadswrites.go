package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadswrite"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// maxLeadWriteBody bounds request bodies; Python reads any size, a lead is a few KiB.
const maxLeadWriteBody = 10 << 20

const leadExistsMessage = "A lead already exists for this company and city. Update the existing lead instead."

type leadWriteRoutes struct{ d Deps }

// registerWrites registers the four lead write routes (routers/leads.py). Every one sits behind
// require_editor = RequireRole("tables.write", "editor", "admin"). Python audits none of them and
// never calls on_row_added (add_lead only upserts), so neither does Go.
func registerWrites(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := leadWriteRoutes{d: d}
	authn := identity.Authenticate(d.Signer, d.Identity)
	editor := []httpx.Middleware{authn, identity.Workspace(d.Identity), identity.RequireRole("tables.write", "editor", "admin")}
	r.Handle("POST /api/lead", http.HandlerFunc(h.create), editor...)
	r.Handle("PUT /api/lead/{lead_id}", http.HandlerFunc(h.update), editor...)
	r.Handle("POST /api/lead/{lead_id}/status", http.HandlerFunc(h.status), editor...)
	r.Handle("DELETE /api/lead/{lead_id}", http.HandlerFunc(h.remove), editor...)
}

func (h leadWriteRoutes) fail(w http.ResponseWriter, what string, err error) {
	l := h.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("leads: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// readBody reads and decodes the JSON body with exact integers. A missing or malformed body maps to the
// FastAPI envelope errors; the decoded value may be of any JSON type.
func readLeadBody(w http.ResponseWriter, r *http.Request) (body any, errs []httpx.FieldError, ok bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLeadWriteBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, nil, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, []httpx.FieldError{{Type: "missing", Loc: []any{"body"}, Msg: "Field required", Input: nil}}, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil || dec.More() {
		return nil, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}, true
	}
	return body, nil, true
}

// modelBody is a pydantic-model body: any non-object is model_attributes_type.
func modelBody(body any, errs []httpx.FieldError) (map[string]any, []httpx.FieldError) {
	if errs != nil {
		return nil, errs
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return nil, []httpx.FieldError{{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body}}
	}
	return obj, nil
}

func (h leadWriteRoutes) create(w http.ResponseWriter, r *http.Request) {
	body, errs, ok := readLeadBody(w, r)
	if !ok {
		return
	}
	obj, errs := modelBody(body, errs)
	var n leadswrite.NewLead
	if errs == nil {
		n = leadswrite.NewLead{
			Company:        fieldString(obj, "company", true, "", &errs),
			City:           fieldString(obj, "city", false, "", &errs),
			Website:        fieldString(obj, "website", false, "", &errs),
			Email:          fieldString(obj, "email", false, "", &errs),
			Phone:          fieldString(obj, "phone", false, "", &errs),
			Specialization: fieldString(obj, "specialization", false, "", &errs),
			ContactPerson:  fieldString(obj, "contact_person", false, "", &errs),
			LinkedinURL:    fieldString(obj, "linkedin_url", false, "", &errs),
			Notes:          fieldString(obj, "notes", false, "", &errs),
			Source:         fieldString(obj, "source", false, "manual", &errs),
		}
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	// A NUL byte is valid for pydantic but rejected by Postgres (Python answers 500); D2 answers 422.
	for name, s := range map[string]string{"company": n.Company, "city": n.City, "website": n.Website, "email": n.Email,
		"phone": n.Phone, "specialization": n.Specialization, "contact_person": n.ContactPerson,
		"linkedin_url": n.LinkedinURL, "notes": n.Notes, "source": n.Source} {
		if strings.IndexByte(s, 0) >= 0 {
			httpx.ValidationError(w, httpx.FieldError{Type: "value_error", Loc: []any{"body", name},
				Msg: "Value error, string cannot contain NUL characters", Input: nil})
			return
		}
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	var id int64
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) (err error) {
		id, err = leadswrite.Create(r.Context(), tx, wc.WorkspaceID, n)
		return err
	})
	var exists *leadswrite.ExistsError
	switch {
	case errors.As(err, &exists):
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"detail": leadExistsDetail{Message: leadExistsMessage, LeadID: exists.LeadID}})
	case err != nil:
		h.fail(w, "create", err)
	default:
		httpx.WriteJSON(w, http.StatusOK, leadCreated{OK: true, ID: id})
	}
}

type leadCreated struct {
	OK bool  `json:"ok"`
	ID int64 `json:"id"`
}

type leadExistsDetail struct {
	Message string `json:"message"`
	LeadID  int64  `json:"lead_id"`
}

type okBody struct {
	OK bool `json:"ok"`
}

// withLead runs fn inside the workspace transaction after the Python get_lead existence check.
// found=false answers 404 `Lead not found`.
func (h leadWriteRoutes) withLead(w http.ResponseWriter, r *http.Request, id int64, none bool, what string,
	fn func(tx pgx.Tx) (bool, error)) {
	if none {
		httpx.Detail(w, http.StatusNotFound, leadNotFound)
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	found := false
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) error {
		if id < 1 || id > 1<<31-1 {
			return nil
		}
		exists, err := gen.New(tx).LeadExists(r.Context(), int32(id))
		if err != nil || !exists {
			return err
		}
		found, err = fn(tx)
		return err
	})
	var fe *leadswrite.FieldError
	switch {
	case errors.As(err, &fe):
		httpx.ValidationError(w, httpx.FieldError{Type: "value_error", Loc: []any{"body", fe.Field},
			Msg: "Value error, " + fe.Reason, Input: nil})
	case err != nil:
		h.fail(w, what, err)
	case !found:
		httpx.Detail(w, http.StatusNotFound, leadNotFound)
	default:
		httpx.WriteJSON(w, http.StatusOK, okBody{OK: true})
	}
}

func (h leadWriteRoutes) update(w http.ResponseWriter, r *http.Request) {
	body, errs, ok := readLeadBody(w, r)
	if !ok {
		return
	}
	var obj map[string]any
	if errs == nil {
		if obj, ok = body.(map[string]any); !ok {
			errs = []httpx.FieldError{{Type: "dict_type", Loc: []any{"body"}, Msg: "Input should be a valid dictionary", Input: body}}
		}
	}
	id, none, ok := pathLeadID(w, r, errs)
	if !ok {
		return
	}
	h.withLead(w, r, id, none, "update", func(tx pgx.Tx) (bool, error) {
		return leadswrite.UpdateFields(r.Context(), tx, id, obj)
	})
}

func (h leadWriteRoutes) status(w http.ResponseWriter, r *http.Request) {
	body, errs, ok := readLeadBody(w, r)
	if !ok {
		return
	}
	obj, errs := modelBody(body, errs)
	var status string
	if errs == nil {
		status = fieldString(obj, "status", true, "", &errs)
		fieldString(obj, "note", false, "", &errs) // validated, then ignored (update_status drops it)
	}
	id, none, ok := pathLeadID(w, r, errs)
	if !ok {
		return
	}
	valid := false
	for _, s := range leadStatuses {
		valid = valid || s == status
	}
	if !valid {
		httpx.Detail(w, http.StatusBadRequest, "Invalid lead status")
		return
	}
	h.withLead(w, r, id, none, "status", func(tx pgx.Tx) (bool, error) {
		return leadswrite.UpdateStatus(r.Context(), tx, id, status)
	})
}

func (h leadWriteRoutes) remove(w http.ResponseWriter, r *http.Request) {
	id, none, ok := pathLeadID(w, r, nil)
	if !ok {
		return
	}
	h.withLead(w, r, id, none, "delete", func(tx pgx.Tx) (bool, error) {
		return leadswrite.Delete(r.Context(), tx, id)
	})
}
