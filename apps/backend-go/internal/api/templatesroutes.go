package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
)

// RegisterTemplates registers routers/templates.py: the recipe gallery
// (GET /gallery, GET /gallery/{slug}, POST /gallery/{slug}/instantiate) and the
// legacy templates (GET "", POST /{template_id}/create). Like FastAPI, the GET
// routes carry no auth dependency (static data); the two POSTs are editor routes.
// Nothing registers unless PG_LEAD_STORE is on.
func RegisterTemplates(r *httpx.Router, d Deps) {
	if !d.workbooksEnabled("templates") {
		return
	}
	reg, err := wbregistry.Load()
	if err != nil {
		l := d.Log
		if l == nil {
			l = slog.Default()
		}
		l.Error("templates: registry unavailable, routes not registered", "err_type", errTypeName(err))
		return
	}
	registerTemplates(r, d, reg)
}

func registerTemplates(r *httpx.Router, d Deps, reg *wbregistry.Registry) {
	h := templateRoutes{reg: reg}
	// Literal gallery routes are more specific than /{template_id}/create.
	r.Handle("GET /api/templates/gallery", http.HandlerFunc(h.gallery))
	r.Handle("GET /api/templates/gallery/{slug}", http.HandlerFunc(h.recipe))
	r.Handle("GET /api/templates", http.HandlerFunc(h.list))
	if d.Pool == nil {
		return
	}
	h.wb = wbCRUDRoutes{d: d, svc: &wbcrud.Service{
		Pool:  d.Pool,
		Leads: wbcrud.PGLeads{},
		Hook:  automationhook.New(d.Flags.AutomationsEnabled, d.Log),
	}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("tables.write", "editor", "admin")
	r.Handle("POST /api/templates/gallery/{slug}/instantiate", http.HandlerFunc(h.instantiate), authn, workspace, editor)
	r.Handle("POST /api/templates/{template_id}/create", http.HandlerFunc(h.createFromTemplate), authn, workspace, editor)
}

type templateRoutes struct {
	reg *wbregistry.Registry
	wb  wbCRUDRoutes
}

func (h templateRoutes) gallery(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	recipes, ok := h.reg.Recipes(category)
	if !ok {
		httpx.Detail(w, http.StatusNotFound, "Unknown category '"+category+"'")
		return
	}
	cards := make([]wbregistry.RecipeSummaryView, 0, len(recipes))
	for _, rc := range recipes {
		cards = append(cards, wbregistry.RecipeSummary(rc))
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Recipes    []wbregistry.RecipeSummaryView `json:"recipes"`
		Categories json.RawMessage                `json:"categories"`
		Total      int                            `json:"total"`
	}{cards, h.reg.RecipeCategories(), len(recipes)})
}

func (h templateRoutes) recipe(w http.ResponseWriter, r *http.Request) {
	rc, ok := h.reg.Recipe(r.PathValue("slug"))
	if !ok {
		httpx.Detail(w, http.StatusNotFound, "Recipe not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rc)
}

func (h templateRoutes) list(w http.ResponseWriter, r *http.Request) {
	ts := h.reg.Templates(r.URL.Query().Get("category"))
	httpx.WriteJSON(w, http.StatusOK, struct {
		Templates  []wbregistry.Template `json:"templates"`
		Categories json.RawMessage       `json:"categories"`
		Total      int                   `json:"total"`
	}{ts, h.reg.TemplateCategories(), len(ts)})
}

// instantiateName parses the optional InstantiateRecipeRequest body ({name: 1..255}).
// An empty body or a JSON null means no body; extra keys are ignored.
func instantiateName(raw []byte) (string, []httpx.FieldError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return "", errs
	}
	v, ok := obj.Get("name")
	if !ok || v == nil {
		return "", nil
	}
	at := []any{"body", "name"}
	s, isStr := v.(string)
	if !isStr {
		return "", []httpx.FieldError{{Type: "string_type", Loc: at, Msg: "Input should be a valid string", Input: v}}
	}
	switch n := utf8.RuneCountInString(s); {
	case n < 1:
		return "", []httpx.FieldError{{Type: "string_too_short", Loc: at, Msg: "String should have at least 1 character",
			Input: v, Ctx: map[string]any{"min_length": 1}}}
	case n > 255:
		return "", []httpx.FieldError{{Type: "string_too_long", Loc: at, Msg: "String should have at most 255 characters",
			Input: v, Ctx: map[string]any{"max_length": 255}}}
	}
	return s, nil
}

func (h templateRoutes) instantiate(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.wb.body(w, r)
	if !ok {
		return
	}
	name, errs := instantiateName(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	rc, ok := h.reg.Recipe(r.PathValue("slug"))
	if !ok {
		httpx.Detail(w, http.StatusNotFound, "Recipe not found")
		return
	}
	var def struct {
		Description string            `json:"description"`
		Columns     []json.RawMessage `json:"columns"`
	}
	if err := json.Unmarshal(rc.Workbook, &def); err != nil {
		h.wb.fail(w, "instantiate", err)
		return
	}
	cols := make([]wbcrud.Column, 0, len(def.Columns))
	for i, c := range def.Columns {
		col, cerrs := wbcrud.NormalizeRequestColumn(c, "columns", i)
		if len(cerrs) > 0 {
			h.wb.fail(w, "instantiate", errBadRecipe)
			return
		}
		cols = append(cols, col)
	}
	if name == "" {
		name = rc.Name
	}
	desc := def.Description
	if desc == "" {
		desc = rc.Description
	}
	wb, err := h.wb.svc.Create(r.Context(), h.wb.wsID(r), wbcrud.CreateBody{
		Name: name, Description: desc, Source: "empty", MaxRows: 1000, Columns: cols,
	})
	if err != nil {
		h.wb.fail(w, "instantiate", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, wb)
}

type recipeError string

func (e recipeError) Error() string { return string(e) }

const errBadRecipe = recipeError("shipped recipe column failed normalisation")

// buildTemplateColumns ports the column assembly of create_from_template: one
// lead_field editor column per template field, then the enrichment columns
// verbatim, guarded like Python (explicit unique enrichment ids, unique ids
// overall, no dependency cycles). The int is the enrichment column count.
func buildTemplateColumns(t wbregistry.Template) ([]wbcrud.Column, int, *wbcrud.GuardError) {
	var fields []struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	if len(t.Columns) > 0 {
		if err := json.Unmarshal(t.Columns, &fields); err != nil {
			return nil, 0, &wbcrud.GuardError{Status: 422, Detail: "Template column IDs must be unique"}
		}
	}
	cols := make([]wbcrud.Column, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		cols = append(cols, wbcrud.MakeColumn("id", f.Key, "name", f.Name, "type", "lead_field",
			"lead_field", f.Key, "width", json.Number("180")))
		seen[f.Key] = true
	}
	badEnrichment := &wbcrud.GuardError{Status: 422,
		Detail: "Template enrichment column IDs must be explicit and unique. Repair the template before creating a workbook."}
	enrichment := 0
	if len(bytes.TrimSpace(t.EnrichmentColumns)) > 0 && string(bytes.TrimSpace(t.EnrichmentColumns)) != "null" {
		parsed, err := wbcrud.ParseColumns(t.EnrichmentColumns)
		if err != nil {
			return nil, 0, badEnrichment
		}
		for _, ec := range parsed {
			id, isStr := ec.Get("id").(string)
			if !isStr || strings.TrimSpace(id) == "" || seen[id] {
				return nil, 0, badEnrichment
			}
			seen[id] = true
			cols = append(cols, ec)
			enrichment++
		}
	}
	ids := map[string]bool{}
	for _, c := range cols {
		id, _ := c.Get("id").(string)
		if ids[id] {
			return nil, 0, &wbcrud.GuardError{Status: 422, Detail: "Template column IDs must be unique"}
		}
		ids[id] = true
	}
	if len(wbcrud.CycleBlockedColumns(cols)) > 0 {
		return nil, 0, &wbcrud.GuardError{Status: 422,
			Detail: "Template has circular column dependencies. Repair the template before creating a workbook."}
	}
	return cols, enrichment, nil
}

func (h templateRoutes) createFromTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("template_id")
	t, ok := h.reg.Template(id)
	if !ok {
		httpx.Detail(w, http.StatusNotFound, "Template not found")
		return
	}
	cols, enrichment, g := buildTemplateColumns(t)
	if g != nil {
		httpx.Detail(w, g.Status, g.Detail)
		return
	}
	colJSON, err := marshalTemplateColumns(cols)
	if err != nil {
		h.wb.fail(w, "create from template", err)
		return
	}
	filter := t.Filter
	if len(bytes.TrimSpace(filter)) == 0 {
		filter = json.RawMessage(`{}`)
	}
	ws := h.wb.wsID(r)
	var wbID string
	// Python adds no rows here, so no automation hook; row_storage_version stays
	// unset (source_config is the model default {}).
	err = h.wb.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		row, err := gen.New(tx).InsertWorkbook(r.Context(), gen.InsertWorkbookParams{
			ID: wbcrud.NewID(), Name: t.Name, Description: t.Description, WorkspaceID: ws,
			SourceType: "empty", SourceConfig: []byte(`{}`), FilterCriteria: filter, ColumnsConfig: colJSON,
		})
		wbID = row.ID
		return err
	})
	if err != nil {
		h.wb.fail(w, "create from template", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		ID                     string `json:"id"`
		Name                   string `json:"name"`
		Template               string `json:"template"`
		ColumnsCount           int    `json:"columns_count"`
		EnrichmentColumnsCount int    `json:"enrichment_columns_count"`
	}{wbID, t.Name, id, len(cols), enrichment})
}

// marshalTemplateColumns is the stored columns_config text, not HTML-escaped like the other write paths.
func marshalTemplateColumns(cols []wbcrud.Column) ([]byte, error) { return wbcrud.MarshalNoEscape(cols) }
