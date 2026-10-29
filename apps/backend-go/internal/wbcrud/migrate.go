package wbcrud

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

// Migration bounds. Python reads every matching lead in one request; Go bounds the
// work per request (documented deviation in testdata/contract/README.md).
const (
	// DefaultMigratePageSize is Python's page size (_query_leads page_size=500).
	DefaultMigratePageSize = 500
	// DefaultMaxMigrateRows caps the leads one migrate request snapshots (413 beyond it).
	DefaultMaxMigrateRows = 100_000
	// MaxMigrateColumns caps the columns of a workbook migrate accepts (413 beyond it).
	MaxMigrateColumns = 1000
)

// MigrateResult is the migrate response: Status is "migrated" or "already_migrated".
type MigrateResult struct {
	Status               string
	Rows                 int
	EnrichmentsMigrated  int
	includeEnrichmentCnt bool
}

// MarshalJSON keeps FastAPI's key order: status, rows[, enrichments_migrated].
func (r MigrateResult) MarshalJSON() ([]byte, error) {
	if r.includeEnrichmentCnt {
		return json.Marshal(struct {
			Status              string `json:"status"`
			Rows                int    `json:"rows"`
			EnrichmentsMigrated int    `json:"enrichments_migrated"`
		}{r.Status, r.Rows, r.EnrichmentsMigrated})
	}
	return json.Marshal(struct {
		Status string `json:"status"`
		Rows   int    `json:"rows"`
	}{r.Status, r.Rows})
}

// Migrate is migrate_workbook_to_v2: a workbook with stored rows is only marked
// row_storage_version 2; otherwise its filtered leads are snapshotted into
// workbook_rows (positions 0.., lead data trimmed to the columns' lead fields plus the
// essentials, v1 enrichment overlay inlined) and source_type/source_config updated.
// Call inside the workspace tx with w locked FOR UPDATE: a concurrent migrate then
// serialises and sees already_migrated. Everything commits or rolls back with that tx.
// Leads are read and inserted one page at a time, so memory stays bounded.
func (s *Service) Migrate(ctx context.Context, tx pgx.Tx, ws string, w Workbook) (MigrateResult, error) {
	q := gen.New(tx)
	existing, err := q.CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: w.ID, WorkspaceID: ws})
	if err != nil {
		return MigrateResult{}, err
	}
	if existing > 0 {
		if err := s.markRowStorage(ctx, tx, ws, w); err != nil {
			return MigrateResult{}, err
		}
		return MigrateResult{Status: "already_migrated", Rows: int(existing)}, nil
	}
	if len(w.Columns) > MaxMigrateColumns {
		return MigrateResult{}, &GuardError{Status: 413, Detail: fmt.Sprintf("Workbook has more than %d columns; migration refused", MaxMigrateColumns)}
	}
	if s.Leads == nil {
		return MigrateResult{}, fmt.Errorf("wbcrud: no lead reader configured")
	}
	colFields := map[string]bool{"id": true, "company": true, "website": true, "email": true, "phone": true, "city": true}
	for _, c := range w.Columns {
		if lf, ok := c.Get("lead_field").(string); ok && lf != "" {
			colFields[lf] = true
		}
	}
	enrichCount, err := q.MigrateCountEnrichments(ctx, gen.MigrateCountEnrichmentsParams{WorkbookID: w.ID, WorkspaceID: ws})
	if err != nil {
		return MigrateResult{}, err
	}
	var fc *Obj
	switch v := w.FilterCriteria.(type) {
	case nil:
	case *Obj:
		fc = v
	default:
		return MigrateResult{}, fmt.Errorf("wbcrud: filter_criteria is not an object")
	}
	filter, err := filterFromObj(fc)
	if err != nil {
		return MigrateResult{}, err
	}
	pageSize, maxRows := s.MigratePageSize, s.MaxMigrateRows
	if pageSize <= 0 {
		pageSize = DefaultMigratePageSize
	}
	if maxRows <= 0 {
		maxRows = DefaultMaxMigrateRows
	}
	tooMany := &GuardError{Status: 413, Detail: fmt.Sprintf("Workbook matches more than %d leads; narrow filter_criteria before migrating", maxRows)}

	migrated := 0
	for page := 1; ; page++ {
		leads, total, err := s.Leads.QueryLeads(ctx, tx, filter, page, pageSize)
		if err != nil {
			return MigrateResult{}, err
		}
		if total > maxRows {
			return MigrateResult{}, tooMany
		}
		if len(leads) == 0 {
			if migrated < total {
				return MigrateResult{}, fmt.Errorf("wbcrud: lead snapshot ended before all matching rows were read")
			}
			break
		}
		if migrated+len(leads) > maxRows {
			return MigrateResult{}, tooMany
		}
		overlay, err := migrateOverlay(ctx, q, ws, w.ID, leads)
		if err != nil {
			return MigrateResult{}, err
		}
		type item struct {
			P int    `json:"p"`
			L int64  `json:"l"`
			D string `json:"d"`
			E string `json:"e"`
		}
		items := make([]item, len(leads))
		for i, l := range leads {
			data, err := trimLead(l, colFields)
			if err != nil {
				return MigrateResult{}, err
			}
			enr := overlay[l.ID]
			if enr == nil {
				enr = NewObj()
			}
			items[i] = item{P: migrated + i, L: l.ID, D: data, E: PyDumps(enr, false)}
		}
		raw, err := json.Marshal(items)
		if err != nil {
			return MigrateResult{}, err
		}
		if err := q.MigrateInsertRows(ctx, gen.MigrateInsertRowsParams{WorkbookID: w.ID, WorkspaceID: ws, Items: raw}); err != nil {
			return MigrateResult{}, err
		}
		migrated += len(leads)
		if migrated >= total {
			break
		}
	}

	cfg := NewObj()
	if fc != nil {
		for _, k := range fc.Keys() {
			v, _ := fc.Get(k)
			cfg.Set(k, v)
		}
	}
	cfg.Set("row_storage_version", json.Number("2"))
	st := "leads_filter"
	if _, _, err := s.UpdateIfChanged(ctx, tx, ws, w, UpdatePatch{SourceType: &st, SetSourceConfig: true, SourceConfig: cfg}); err != nil {
		return MigrateResult{}, err
	}
	return MigrateResult{Status: "migrated", Rows: migrated, EnrichmentsMigrated: int(enrichCount), includeEnrichmentCnt: true}, nil
}

// trimLead is _trim: the lead's keys (in lead order) restricted to the column fields.
func trimLead(l leadsread.Lead, keep map[string]bool) (string, error) {
	raw, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	full, err := ParseObject(raw)
	if err != nil {
		return "", err
	}
	out := NewObj()
	for _, k := range full.Keys() {
		if keep[k] {
			v, _ := full.Get(k)
			out.Set(k, v)
		}
	}
	return PyDumps(out, false), nil
}

// migrateOverlay builds {lead_id: {column_id: {value, status, provider, error}}} for one page.
func migrateOverlay(ctx context.Context, q *gen.Queries, ws, wbID string, leads []leadsread.Lead) (map[int64]*Obj, error) {
	ids := make([]int32, 0, len(leads))
	for _, l := range leads {
		if l.ID >= -1<<31 && l.ID <= 1<<31-1 {
			ids = append(ids, int32(l.ID))
		}
	}
	rows, err := q.MigrateEnrichmentsForLeads(ctx, gen.MigrateEnrichmentsForLeadsParams{WorkbookID: wbID, WorkspaceID: ws, LeadIds: ids})
	if err != nil {
		return nil, err
	}
	out := map[int64]*Obj{}
	for _, r := range rows {
		o := out[int64(r.LeadID)]
		if o == nil {
			o = NewObj()
			out[int64(r.LeadID)] = o
		}
		cell := NewObj()
		cell.Set("value", textOrNil(r.Value.String, r.Value.Valid))
		status := "pending"
		if r.Status.Valid && r.Status.String != "" {
			status = r.Status.String
		}
		cell.Set("status", status)
		cell.Set("provider", textOrNil(r.Provider.String, r.Provider.Valid))
		cell.Set("error", textOrNil(r.Error.String, r.Error.Valid))
		o.Set(r.ColumnID, cell)
	}
	return out, nil
}

func textOrNil(s string, valid bool) any {
	if !valid {
		return nil
	}
	return s
}
