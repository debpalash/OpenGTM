package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbfunctions"
)

const maxFunctionsBody = 8 << 20

// RegisterFunctions registers /api/functions (routers/functions.py). Every
// route is behind get_current_active_user; create/delete also need a global
// admin; apply needs a workspace and the tables.write editor gate.
func RegisterFunctions(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	f := fnRoutes{d: d, store: wbfunctions.New(d.Pool)}
	authn := identity.Authenticate(d.Signer, d.Identity)
	admin := []httpx.Middleware{authn, identity.RequireGlobalAdmin()}
	r.Handle("GET /api/functions", http.HandlerFunc(f.list), authn)
	r.Handle("POST /api/functions", http.HandlerFunc(f.create), admin...)
	r.Handle("GET /api/functions/{func_id}", http.HandlerFunc(f.get), authn)
	r.Handle("POST /api/functions/{func_id}/apply", http.HandlerFunc(f.apply),
		authn, identity.Workspace(d.Identity), identity.RequireRole("tables.write", "editor", "admin"))
	r.Handle("DELETE /api/functions/{func_id}", http.HandlerFunc(f.remove), admin...)
}

type fnRoutes struct {
	d     Deps
	store *wbfunctions.Store
}

func (f fnRoutes) fail(w http.ResponseWriter, what string, err error) {
	l := f.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("functions: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func errTypeName(err error) string { return fmt.Sprintf("%T", err) }

func fnTextPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// chainOrEmpty mirrors `_chain`: a JSON null chain reads as [].
func chainOrEmpty(raw []byte) json.RawMessage {
	if t := bytes.TrimSpace(raw); len(t) == 0 || string(t) == "null" {
		return json.RawMessage("[]")
	}
	return json.RawMessage(raw)
}

func chainCount(raw json.RawMessage) int {
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) == nil {
		return len(a)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return len(m)
}

type fnListItem struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  *string         `json:"description"`
	Category     *string         `json:"category"`
	ColumnsChain json.RawMessage `json:"columns_chain"`
	ColumnsCount int             `json:"columns_count"`
	Version      *int32          `json:"version"`
	UsageCount   *int32          `json:"usage_count"`
	CreatedAt    *pyFloat        `json:"created_at"`
}

type fnDetail struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  *string         `json:"description"`
	Category     *string         `json:"category"`
	ColumnsChain json.RawMessage `json:"columns_chain"`
	Version      *int32          `json:"version"`
	UsageCount   *int32          `json:"usage_count"`
}

func (f fnRoutes) list(w http.ResponseWriter, r *http.Request) {
	cat := ""
	if vs := r.URL.Query()["category"]; len(vs) > 0 {
		cat = vs[len(vs)-1]
	}
	rows, err := f.store.List(r.Context(), cat)
	if err != nil {
		f.fail(w, "list", err)
		return
	}
	items := make([]fnListItem, 0, len(rows))
	for _, row := range rows {
		chain := chainOrEmpty(row.ColumnsChain)
		it := fnListItem{
			ID: row.ID, Name: row.Name, Description: fnTextPtr(row.Description), Category: fnTextPtr(row.Category),
			ColumnsChain: chain, ColumnsCount: chainCount(chain),
		}
		if row.Version.Valid {
			v := row.Version.Int32
			it.Version = &v
		}
		if row.UsageCount.Valid {
			v := row.UsageCount.Int32
			it.UsageCount = &v
		}
		if row.CreatedAt.Valid {
			v := pyFloat(row.CreatedAt.Float64)
			it.CreatedAt = &v
		}
		items = append(items, it)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"functions": items, "total": len(items)})
}

func (f fnRoutes) get(w http.ResponseWriter, r *http.Request) {
	row, err := f.store.Get(r.Context(), r.PathValue("func_id"))
	if errors.Is(err, wbfunctions.ErrFunctionNotFound) {
		httpx.Detail(w, http.StatusNotFound, "Function not found")
		return
	}
	if err != nil {
		f.fail(w, "get", err)
		return
	}
	out := fnDetail{
		ID: row.ID, Name: row.Name, Description: fnTextPtr(row.Description), Category: fnTextPtr(row.Category),
		ColumnsChain: chainOrEmpty(row.ColumnsChain),
	}
	if row.Version.Valid {
		v := row.Version.Int32
		out.Version = &v
	}
	if row.UsageCount.Valid {
		v := row.UsageCount.Int32
		out.UsageCount = &v
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (f fnRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := f.store.Delete(r.Context(), r.PathValue("func_id")); err != nil {
		f.fail(w, "delete", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readJSONObject reads and decodes a request body into a JSON object with
// pydantic's envelope errors (missing body, bad JSON, non-object).
func readJSONObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFunctionsBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		httpx.ValidationError(w, httpx.FieldError{Type: "missing", Loc: []any{"body"}, Msg: "Field required", Input: nil})
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body any
	if err := dec.Decode(&body); err != nil || dec.More() {
		httpx.ValidationError(w, httpx.FieldError{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}})
		return nil, false
	}
	obj, ok := body.(map[string]any)
	if !ok {
		httpx.ValidationError(w, httpx.FieldError{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body})
		return nil, false
	}
	return obj, true
}

func fieldString(obj map[string]any, key string, required bool, def string, errs *[]httpx.FieldError) string {
	v, present := obj[key]
	if !present {
		if required {
			*errs = append(*errs, httpx.FieldError{Type: "missing", Loc: []any{"body", key}, Msg: "Field required", Input: obj})
		}
		return def
	}
	s, ok := v.(string)
	if !ok {
		*errs = append(*errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", key}, Msg: "Input should be a valid string", Input: v})
		return def
	}
	return s
}

func (f fnRoutes) create(w http.ResponseWriter, r *http.Request) {
	obj, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	var errs []httpx.FieldError
	name := fieldString(obj, "name", true, "", &errs)
	desc := fieldString(obj, "description", false, "", &errs)
	cat := fieldString(obj, "category", false, "custom", &errs)
	var chain []any
	if v, present := obj["columns_chain"]; !present {
		errs = append(errs, httpx.FieldError{Type: "missing", Loc: []any{"body", "columns_chain"}, Msg: "Field required", Input: obj})
	} else if list, isList := v.([]any); !isList {
		errs = append(errs, httpx.FieldError{Type: "list_type", Loc: []any{"body", "columns_chain"}, Msg: "Input should be a valid list", Input: v})
	} else {
		chain = list
		for i, el := range list {
			if _, isObj := el.(map[string]any); !isObj {
				errs = append(errs, httpx.FieldError{Type: "dict_type", Loc: []any{"body", "columns_chain", i}, Msg: "Input should be a valid dictionary", Input: el})
			}
		}
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if chain == nil {
		chain = []any{}
	}
	if err := enc.Encode(chain); err != nil {
		f.fail(w, "encode chain", err)
		return
	}
	id, err := f.store.Create(r.Context(), name, desc, cat, bytes.TrimSpace(buf.Bytes()))
	if err != nil {
		f.fail(w, "create", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "columns_count": len(chain)})
}

func (f fnRoutes) apply(w http.ResponseWriter, r *http.Request) {
	obj, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	var errs []httpx.FieldError
	wbID := fieldString(obj, "workbook_id", true, "", &errs)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	res, err := f.store.Apply(r.Context(), ws.WorkspaceID, r.PathValue("func_id"), wbID)
	switch {
	case errors.Is(err, wbfunctions.ErrWorkbookNotFound):
		httpx.Detail(w, http.StatusNotFound, "Workbook not found")
	case errors.Is(err, wbfunctions.ErrFunctionNotFound):
		httpx.Detail(w, http.StatusNotFound, "Function not found")
	case err != nil:
		f.fail(w, "apply", err)
	default:
		httpx.WriteJSON(w, http.StatusOK, res)
	}
}
