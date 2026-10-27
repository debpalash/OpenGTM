package sourcewb

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// CRM import limits of crm_import.py.
const (
	CRMDefaultLimit = 500
	CRMMaxLimit     = 5000
)

var (
	crmRowFields  = []string{"company", "website", "email", "phone", "city", "state", "address", "contact_person", "contact_title", "description", "linkedin_url", "score"}
	crmSyncFields = []string{"crm_external_id", "crm_type", "crm_object"}
)

// CRMEffectiveLimit is crm_import.effective_limit: default 500, capped at 5000, floor 1.
func CRMEffectiveLimit(cfg map[string]any) int {
	limit := int64(CRMDefaultLimit)
	if v := cfg["limit"]; truthy(v) {
		if n, ok := pyInt(v); ok {
			limit = n
		}
	}
	return int(max(1, min(limit, CRMMaxLimit)))
}

func emptyValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

func fieldMapOf(v any) crm.FieldMap {
	o, ok := asObj(v)
	if !ok || o.Len() == 0 {
		return nil
	}
	var fm crm.FieldMap
	for _, k := range o.Keys() {
		fm = append(fm, crm.FieldPair{Key: k, Value: pyStr(get(o, k))})
	}
	return fm
}

// fetch is crm_import._fetch.
func (d Deps) fetchCRM(ctx context.Context, ws string, cfg map[string]any) (res crm.FetchResult) {
	name := strings.ToLower(orStr(cfg["crm"]))
	object := "contact"
	if truthy(cfg["object"]) {
		object = strings.ToLower(pyStr(cfg["object"]))
	}
	if object != "contact" {
		return crm.FetchResult{Error: fmt.Sprintf("unsupported crm object '%s' (only 'contact' in v1)", object)}
	}
	if name != "hubspot" && name != "salesforce" {
		return crm.FetchResult{Error: fmt.Sprintf("unsupported crm '%s'", name)}
	}
	defer func() { // defensive: adapters return errors as data, but never leak a panic
		if r := recover(); r != nil {
			res = crm.FetchResult{Error: truncRunes(fmt.Sprint(r), 200)}
		}
	}()
	return d.CRM.FetchContacts(ctx, ws, name, CRMEffectiveLimit(cfg), orStr(cfg["filter"]), fieldMapOf(cfg["field_map"]))
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// crmRowData is the row snapshot of a contact: lead fields first, then any other
// non-empty keys (field_map targets; sorted, as Go maps carry no order), then the sync
// key fields as strings.
func crmRowData(c map[string]any) *wbcrud.Obj {
	data := wbcrud.NewObj()
	for _, k := range crmRowFields {
		if v, ok := c[k]; ok && !emptyValue(v) {
			data.Set(k, v)
		}
	}
	sync := map[string]bool{}
	for _, k := range crmSyncFields {
		sync[k] = true
	}
	var extra []string
	for k, v := range c {
		if _, set := data.Get(k); !set && !sync[k] && !emptyValue(v) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		data.Set(k, c[k])
	}
	for _, k := range crmSyncFields {
		if truthy(c[k]) {
			data.Set(k, pyStr(c[k]))
		}
	}
	return data
}

// materializeCRM is crm_import.materialize_crm_import: pull contacts and append one row
// per new (crm_type, crm_external_id). Contacts are person-level and are not resolved into
// the company entity graph.
func (d Deps) materializeCRM(ctx context.Context, ws, wbID, colID string, cfg map[string]any) (*wbcrud.Obj, error) {
	if !d.CRMImportEnabled {
		return obj("error", "crm_import_disabled", "found", 0, "added", 0,
			"detail", "CRM import is disabled (set CRM_IMPORT_ENABLED=true)"), nil
	}
	name := strings.ToLower(orStr(cfg["crm"]))
	if err := d.setStatus(ctx, ws, wbID, "running"); err != nil {
		return nil, err
	}
	res := d.fetchCRM(ctx, ws, cfg)
	if !res.Success {
		if err := d.setStatus(ctx, ws, wbID, "draft"); err != nil {
			return nil, err
		}
		msg := truncRunes(res.Error, 200)
		if msg == "" {
			msg = "crm import failed"
		}
		return obj("error", msg, "found", 0, "added", 0, "crm", name), nil
	}
	contacts := res.Contacts
	out, err := d.runWrites(ctx, ws, wbID, colID, len(contacts), func(tx pgx.Tx, q *gen.Queries, w *writeOut) error {
		rows, err := q.SWRowsData(ctx, gen.SWRowsDataParams{WorkbookID: wbID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		present := map[[2]string]bool{}
		pos := int32(0)
		for _, r := range rows {
			data, _ := wbcrud.ParseObject(nonEmptyJSON(r.Data))
			if ext := get(data, "crm_external_id"); truthy(ext) {
				present[[2]string{strings.ToLower(orStr(get(data, "crm_type"))), pyStr(ext)}] = true
			}
		}
		stats, err := q.SWRowStats(ctx, gen.SWRowStatsParams{WorkbookID: wbID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		pos = stats.MaxPos
		for _, c := range contacts {
			ext := strip(orStr(c["crm_external_id"]))
			if ext == "" {
				w.skipped++
				continue
			}
			key := [2]string{strings.ToLower(orStr(c["crm_type"])), ext}
			if present[key] {
				w.skipped++
				continue
			}
			present[key] = true
			data := crmRowData(c)
			raw, err := data.MarshalJSON()
			if err != nil {
				return err
			}
			pos++
			id, err := q.SWInsertRow(ctx, gen.SWInsertRowParams{WorkbookID: wbID, WorkspaceID: ws, Position: pos, Data: raw,
				CorroborationCount: pgtype.Int4{Int32: 1, Valid: true}})
			if err != nil {
				return fmt.Errorf("sourcewb: insert row: %w", err)
			}
			w.added++
			w.rowIDs = append(w.rowIDs, int64(id))
			w.events = append(w.events, map[string]any{"type": "row_added", "rowId": int64(id), "data": data})
		}
		return q.SWFinishWorkbook(ctx, gen.SWFinishWorkbookParams{ID: wbID, WorkspaceID: ws})
	})
	if err != nil {
		return nil, err
	}
	d.log().Info("crm import done", "workbook_id", wbID, "column_id", colID, "crm", name,
		"found", len(contacts), "added", out.added, "skipped", out.skipped)
	return obj("found", len(contacts), "added", out.added, "skipped", out.skipped, "crm", name,
		"added_row_ids", idsAny(out.rowIDs)), nil
}
