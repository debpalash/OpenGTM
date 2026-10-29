package wbcrud

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

// Workbook detail (workbooks.py get_workbook): a page of rows with their
// enrichment overlays, from workbook_rows (v2) or, for legacy leads_filter
// workbooks without rows, from the lead store. The response is an ordered *Obj
// in the key order of WorkbookWithLeadsResponse.

// DetailParams are the validated query parameters of GET /{id}.
type DetailParams struct {
	Page, PageSize int64
	ViewID         string
	Search         string
	CursorMode     bool
	// Cursor is nil when the parameter is absent; an empty value still selects keyset paging
	// (`cursor is not None`) but adds no condition.
	Cursor *string
}

// detailQuery is _workbook_rows_query: the saved view (404 when missing) and
// the search term applied to the workbook's rows.
func (s *Service) detailQuery(ctx context.Context, tx pgx.Tx, wb Workbook, viewID, search string) (RowQuery, error) {
	var view *QueryView
	if viewID != "" {
		v, g, err := LoadView(ctx, tx, wb.ID, viewID)
		if err != nil {
			return RowQuery{}, err
		}
		if g != nil {
			return RowQuery{}, g
		}
		view = v
	}
	q, g := BuildRowQuery(QueryWorkbook{ID: wb.ID, Columns: wb.Columns}, view, search)
	if g != nil {
		return RowQuery{}, g
	}
	return q, nil
}

// Detail returns the workbook with one page of rows. Missing or foreign
// workbooks are ErrNotFound; cursor and view problems are *GuardError.
func (s *Service) Detail(ctx context.Context, ws, workbookID string, p DetailParams) (*Obj, error) {
	var out *Obj
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		wb, err := s.Owned(ctx, tx, ws, workbookID, false)
		if err != nil {
			return err
		}
		count, err := gen.New(tx).CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: wb.ID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		if count > 0 || !wb.UsesLegacyLeads() {
			out, err = s.detailRows(ctx, tx, ws, wb, int(count), p)
		} else {
			out, err = s.detailLegacy(ctx, tx, wb, p)
		}
		return err
	})
	return out, err
}

type detailRow struct {
	id         int32
	positionC  int32
	position   *int32
	data, enr  []byte
	leadID     *int32
	canonical  *string
	corr       *int32
	provider   *string
	recordID   *string
	rank       *int32
	fetchedAt  *time.Time
	workbookID string
}

func (r detailRow) cellKey() int64 {
	if r.leadID != nil {
		return int64(*r.leadID)
	}
	return int64(r.id)
}

func (s *Service) detailRows(ctx context.Context, tx pgx.Tx, ws string, wb Workbook, v2Count int, p DetailParams) (*Obj, error) {
	q, err := s.detailQuery(ctx, tx, wb, p.ViewID, p.Search)
	if err != nil {
		return nil, err
	}
	countSQL, countArgs := q.CountSQL()
	var queryTotal int64
	if err := tx.QueryRow(ctx, countSQL, countArgs...).Scan(&queryTotal); err != nil {
		return nil, err
	}
	using := p.CursorMode || p.Cursor != nil
	if p.Cursor != nil && *p.Cursor != "" {
		vals, g, err := q.ResolveCursor(ctx, tx, *p.Cursor)
		if err != nil {
			return nil, err
		}
		if g != nil {
			return nil, g
		}
		if q, g = q.After(vals); g != nil {
			return nil, g
		}
	}
	offset := int64(0)
	if using {
		q = q.WithLimit(int(p.PageSize)+1, 0)
	} else {
		offset = math.MaxInt64
		if p.Page-1 <= math.MaxInt64/p.PageSize {
			offset = (p.Page - 1) * p.PageSize
		}
		q = q.WithLimit(int(p.PageSize), int(offset))
	}
	fetched, err := scanDetailRows(ctx, tx, q.SQL, q.Args)
	if err != nil {
		return nil, err
	}
	var hasMore bool
	rows := fetched
	if using {
		hasMore = int64(len(fetched)) > p.PageSize
		if hasMore {
			rows = fetched[:p.PageSize]
		}
	} else {
		hasMore = offset+int64(len(rows)) < queryTotal
	}
	var nextCursor any
	if using && hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		c, err := q.NextCursor(ctx, tx, int64(last.id), int(last.positionC))
		if err != nil {
			return nil, err
		}
		nextCursor = c
	}

	receipts, err := researchReceipts(ctx, tx, ws, wb, rows)
	if err != nil {
		return nil, err
	}
	outRows := make([]any, len(rows))
	for i, r := range rows {
		data := objOrEmpty(r.data)
		row := NewObj()
		row.Set("lead_id", nullInt(r.leadID))
		row.Set("row_id", int64(r.id))
		row.Set("position", nullInt(r.position))
		row.Set("source_provider", nullStr(r.provider))
		row.Set("source_record_id", nullStr(r.recordID))
		row.Set("source_rank", nullInt(r.rank))
		row.Set("source_fetched_at", awareTime(r.fetchedAt))
		row.Set("lead", data)
		row.Set("data", data)
		row.Set("enrichments", overlaysOf(objOrEmpty(r.enr), r.cellKey(), receipts))
		row.Set("canonical_entity_id", nullStr(r.canonical))
		row.Set("corroboration_count", nullInt(r.corr))
		outRows[i] = row
	}
	wb.TotalRows = v2Count
	res := NewObj()
	res.Set("workbook", wb)
	res.Set("rows", outRows)
	res.Set("total_rows", int64(v2Count))
	res.Set("query_total_rows", queryTotal)
	res.Set("page", p.Page)
	res.Set("page_size", p.PageSize)
	res.Set("next_cursor", nextCursor)
	res.Set("has_more", hasMore)
	return res, nil
}

func scanDetailRows(ctx context.Context, tx pgx.Tx, sql string, args []any) ([]detailRow, error) {
	rs, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []detailRow
	for rs.Next() {
		var r detailRow
		var createdAt, updatedAt *time.Time
		var wsID string
		if err := rs.Scan(&r.id, &r.workbookID, &r.positionC, &r.position, &r.data, &r.enr, &r.leadID, &r.canonical, &r.corr,
			&createdAt, &updatedAt, &wsID, &r.provider, &r.recordID, &r.rank, &r.fetchedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func nullInt(p *int32) any {
	if p == nil {
		return nil
	}
	return int64(*p)
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// awareTime formats a timestamptz like pydantic does for a UTC datetime:
// "2006-01-02T15:04:05[.ffffff]Z".
func awareTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	u := t.UTC()
	s := u.Format("2006-01-02T15:04:05")
	if us := u.Nanosecond() / 1000; us != 0 {
		s += u.Format(".000000")
	}
	return s + "Z"
}

type receiptKey struct {
	lead int64
	col  string
}

type receipt struct {
	value, status *string
	research      *Obj
}

// researchReceipts reads the research cell receipts that can complete the
// overlays of rows: same workbook and workspace, research columns only, and
// only receipts whose metadata carries a research object. A later receipt
// (higher id) wins over an earlier one for the same cell.
func researchReceipts(ctx context.Context, tx pgx.Tx, ws string, wb Workbook, rows []detailRow) (map[receiptKey]receipt, error) {
	out := map[receiptKey]receipt{}
	var cols []string
	for _, c := range wb.Columns {
		if t, _ := c.Get("type").(string); t == "research" {
			if v, ok := c.Lookup("id"); ok && truthy(v) {
				cols = append(cols, pyText(v))
			}
		}
	}
	keys := make([]int32, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, int32(r.cellKey()))
	}
	if len(keys) == 0 || len(cols) == 0 {
		return out, nil
	}
	rs, err := tx.Query(ctx, `SELECT lead_id, column_id, value, status, cell_metadata FROM workbook_enrichments
		WHERE workbook_id = $1 AND workspace_id = $2 AND lead_id = ANY($3::int4[]) AND column_id = ANY($4::text[]) ORDER BY id`,
		wb.ID, ws, keys, cols)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var lead int32
		var col string
		var r receipt
		var meta []byte
		if err := rs.Scan(&lead, &col, &r.value, &r.status, &meta); err != nil {
			return nil, err
		}
		md, _ := decodeMeta(meta)
		if md == nil {
			continue
		}
		if rv, ok := md.Get("research"); ok {
			if ro, ok := rv.(*Obj); ok {
				r.research = ro
				out[receiptKey{int64(lead), col}] = r
			}
		}
	}
	return out, rs.Err()
}

// decodeMeta decodes a cell_metadata value; nil unless it is a JSON object.
func decodeMeta(raw []byte) (*Obj, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	v, err := decodeDocument(raw, true)
	if err != nil {
		return nil, false
	}
	o, ok := v.(*Obj)
	return o, ok
}

// overlaysOf is the per-row enrichments dict: every stored overlay as an
// EnrichmentOverlay, completed with a matching research receipt.
func overlaysOf(enr *Obj, cellKey int64, receipts map[receiptKey]receipt) *Obj {
	out := NewObj()
	for _, col := range enr.Keys() {
		ov, _ := enr.Get(col)
		o, ok := ov.(*Obj)
		if !ok {
			out.Set(col, overlayOut(ov, "complete", nil, nil, nil, nil, nil))
			continue
		}
		var research any
		if _, has := o.Get("research"); has {
			research, _ = o.Get("research")
		} else if rc, ok := receipts[receiptKey{cellKey, col}]; ok {
			value, _ := o.Get("value")
			var serialized *string
			switch t := value.(type) {
			case nil:
			case string:
				serialized = &t
			default:
				s := pyDumps(t, false)
				serialized = &s
			}
			status, _ := o.Get("status")
			if sameText(serialized, rc.value) && sameStatus(status, rc.status) {
				research = rc.research
			}
		}
		val, _ := o.Get("value")
		status := "pending"
		if s, ok := o.Get("status"); ok {
			if str, isStr := s.(string); isStr {
				status = str
			}
		}
		prov, _ := o.Get("provider")
		er, _ := o.Get("error")
		vs, _ := o.Get("verify_status")
		pv, _ := o.Get("provenance")
		out.Set(col, overlayOut(val, status, prov, er, vs, pv, research))
	}
	return out
}

func sameText(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameStatus(v any, status *string) bool {
	if v == nil {
		return status == nil
	}
	s, ok := v.(string)
	return ok && status != nil && s == *status
}

// overlayOut builds an EnrichmentOverlay: value, status, provider, error,
// verify_status, provenance, research. Fields of the wrong type read as null
// (FastAPI would answer 500 for such stored data).
func overlayOut(value any, status string, provider, errText, verify, provenance, research any) *Obj {
	o := NewObj()
	o.Set("value", value)
	o.Set("status", status)
	o.Set("provider", strOrNil(provider))
	o.Set("error", strOrNil(errText))
	o.Set("verify_status", strOrNil(verify))
	o.Set("provenance", provenanceOut(provenance))
	o.Set("research", researchOut(research))
	return o
}

func strOrNil(v any) any {
	if s, ok := v.(string); ok {
		return s
	}
	return nil
}

// floatOrNil is an Optional[float] field: a JSON number (or numeric string)
// rendered as a Python float.
func floatOrNil(v any) (any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case json.Number:
		if f, err := strconv.ParseFloat(string(t), 64); err == nil && !math.IsInf(f, 0) {
			return json.Number(pyFloat(f)), true
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return json.Number(pyFloat(f)), true
		}
	}
	return nil, false
}

// provenanceOut is the Provenance model: source (required), license
// (default "unknown"), confidence, fetched_at; nil when v is not a valid one.
func provenanceOut(v any) any {
	o, ok := v.(*Obj)
	if !ok {
		return nil
	}
	src, ok := o.Get("source")
	if _, isStr := src.(string); !ok || !isStr {
		return nil
	}
	license := any("unknown")
	if l, has := o.Get("license"); has {
		if _, isStr := l.(string); !isStr {
			return nil
		}
		license = l
	}
	cv, _ := o.Get("confidence")
	conf, ok := floatOrNil(cv)
	if !ok {
		return nil
	}
	fa, _ := o.Get("fetched_at")
	if _, isStr := fa.(string); fa != nil && !isStr {
		return nil
	}
	out := NewObj()
	out.Set("source", src)
	out.Set("license", license)
	out.Set("confidence", conf)
	out.Set("fetched_at", fa)
	return out
}

// researchOut is the ResearchEvidence model; nil when v is not a valid one.
func researchOut(v any) any {
	o, ok := v.(*Obj)
	if !ok {
		return nil
	}
	answer := any("")
	if a, has := o.Get("answer"); has {
		if _, isStr := a.(string); !isStr {
			return nil
		}
		answer = a
	}
	cites := []any{}
	if cv, has := o.Get("citations"); has {
		list, isList := cv.([]any)
		if !isList {
			return nil
		}
		for _, c := range list {
			co, isObj := c.(*Obj)
			if !isObj {
				return nil
			}
			cite := NewObj()
			url, _ := co.Get("url")
			if _, isStr := url.(string); !isStr {
				return nil
			}
			cite.Set("url", url)
			for _, f := range []struct{ key, def string }{{"title", ""}, {"quoted_text", ""}} {
				val := any(f.def)
				if x, has := co.Get(f.key); has {
					if _, isStr := x.(string); !isStr {
						return nil
					}
					val = x
				}
				cite.Set(f.key, val)
			}
			fa, _ := co.Get("fetched_at")
			if _, isStr := fa.(string); fa != nil && !isStr {
				return nil
			}
			cite.Set("fetched_at", fa)
			cites = append(cites, cite)
		}
	}
	cost, _ := o.Get("cost_usd")
	costOut, ok := floatOrNil(cost)
	if !ok {
		return nil
	}
	stopped, _ := o.Get("stopped_reason")
	if _, isStr := stopped.(string); stopped != nil && !isStr {
		return nil
	}
	fallback := false
	if f, has := o.Get("synthesis_fallback"); has {
		switch t := f.(type) {
		case bool:
			fallback = t
		case json.Number:
			fallback = string(t) == "1"
		default:
			return nil
		}
	}
	out := NewObj()
	out.Set("answer", answer)
	out.Set("citations", cites)
	out.Set("cost_usd", costOut)
	out.Set("stopped_reason", stopped)
	out.Set("synthesis_fallback", fallback)
	return out
}

// detailLegacy reads a legacy leads_filter workbook that has no rows of its
// own: a page of leads with the workbook_enrichments of those leads.
func (s *Service) detailLegacy(ctx context.Context, tx pgx.Tx, wb Workbook, p DetailParams) (*Obj, error) {
	var f leadsread.Filter
	if o, ok := wb.FilterCriteria.(*Obj); ok {
		if g, err := filterFromObj(o); err == nil {
			f = g
		}
	}
	page := p.Page
	if page > math.MaxInt32 {
		page = math.MaxInt32
	}
	leads, total, err := s.Leads.QueryLeads(ctx, tx, f, int(page), int(p.PageSize))
	if err != nil {
		return nil, err
	}
	byLead := map[int64]*Obj{}
	if len(leads) > 0 {
		ids := make([]int32, len(leads))
		for i, l := range leads {
			ids[i] = int32(l.ID)
		}
		rs, err := tx.Query(ctx, `SELECT lead_id, column_id, value, status, provider, error, cell_metadata FROM workbook_enrichments
			WHERE workbook_id = $1 AND lead_id = ANY($2::int4[]) ORDER BY id`, wb.ID, ids)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		for rs.Next() {
			var lead int32
			var col string
			var value, status, provider, errText *string
			var meta []byte
			if err := rs.Scan(&lead, &col, &value, &status, &provider, &errText, &meta); err != nil {
				return nil, err
			}
			st := "pending"
			if status != nil && *status != "" {
				st = *status
			}
			var verify, prov, research any
			if md, ok := decodeMeta(meta); ok {
				if vo, ok := md.Get("verify"); ok && truthy(vo) {
					if vobj, isObj := vo.(*Obj); isObj {
						verify, _ = vobj.Get("status")
					}
				}
				prov, _ = md.Get("provenance")
				research, _ = md.Get("research")
			}
			m := byLead[int64(lead)]
			if m == nil {
				m = NewObj()
				byLead[int64(lead)] = m
			}
			m.Set(col, overlayOut(nullStr(value), st, nullStr(provider), nullStr(errText), verify, prov, research))
		}
		if err := rs.Err(); err != nil {
			return nil, err
		}
	}
	rows := make([]any, len(leads))
	for i, l := range leads {
		en := byLead[l.ID]
		if en == nil {
			en = NewObj()
		}
		row := NewObj()
		row.Set("lead_id", l.ID)
		row.Set("row_id", nil)
		row.Set("position", nil)
		row.Set("source_provider", nil)
		row.Set("source_record_id", nil)
		row.Set("source_rank", nil)
		row.Set("source_fetched_at", nil)
		row.Set("lead", l)
		row.Set("data", l)
		row.Set("enrichments", en)
		row.Set("canonical_entity_id", nil)
		row.Set("corroboration_count", nil)
		rows[i] = row
	}
	wb.TotalRows = 0
	res := NewObj()
	res.Set("workbook", wb)
	res.Set("rows", rows)
	res.Set("total_rows", int64(total))
	res.Set("query_total_rows", int64(total))
	res.Set("page", p.Page)
	res.Set("page_size", p.PageSize)
	res.Set("next_cursor", nil)
	res.Set("has_more", p.Page <= math.MaxInt64/p.PageSize && p.Page*p.PageSize < int64(total))
	return res, nil
}
