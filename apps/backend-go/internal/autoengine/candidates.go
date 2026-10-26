package autoengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/safecond"
)

// rowRec is the slice of a workbook_rows row the engine reads.
type rowRec struct {
	ID          int32
	LeadID      *int64
	Data        []byte
	Enrichments []byte
}

// candidate is one (workbook, row) of an eval with its workbook's columns.
type candidate struct {
	workbookID string
	rowID      string // str(row.id)
	row        rowRec
	cols       []autoactions.Column
	sc         []safecond.Column
	defaults   map[string][]string
}

// decodeObject reads a JSON object with numbers kept as json.Number (UseNumber,
// so ints and floats keep their Python types). Anything else is an empty map.
func decodeObject(raw []byte) map[string]any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// rowCells is _row_cells: the lead fields of `data`, then the enrichment
// overlay (a {"value": v} cell is unwrapped), overlay winning on conflict.
func rowCells(r rowRec) map[string]any {
	cells := map[string]any{}
	for k, v := range decodeObject(r.Data) {
		cells[k] = v
	}
	for cid, cell := range decodeObject(r.Enrichments) {
		if m, ok := cell.(map[string]any); ok {
			if v, has := m["value"]; has {
				cells[cid] = v
				continue
			}
		}
		cells[cid] = cell
	}
	return cells
}

// leadDataFor is _lead_data_for_action: the cells plus id (lead_id or the row
// id) and lead_id when the row has a lead. Existing keys win (setdefault).
func leadDataFor(r rowRec) map[string]any {
	data := rowCells(r)
	var lead int64
	if r.LeadID != nil {
		lead = *r.LeadID
	}
	id := int64(r.ID)
	if lead != 0 {
		id = lead
	}
	if _, ok := data["id"]; !ok {
		data["id"] = id
	}
	if lead != 0 {
		if _, ok := data["lead_id"]; !ok {
			data["lead_id"] = lead
		}
	}
	return data
}

// parseColumns reads a columns_config JSON array in key order; an unreadable
// value is no columns (Python `columns_config or []`).
func parseColumns(raw []byte) ([]autoactions.Column, []safecond.Column) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	cols, err := colgraph.ParseCols(raw)
	if err != nil {
		return nil, nil
	}
	sc := make([]safecond.Column, 0, len(cols))
	for _, c := range cols {
		id, _ := c.Get("id").(string)
		name, _ := c.Get("name").(string)
		sc = append(sc, safecond.Column{ID: id, Name: name})
	}
	return cols, sc
}

func newCandidate(wb string, r rowRec, cols []autoactions.Column, sc []safecond.Column) candidate {
	return candidate{workbookID: wb, rowID: strconv.FormatInt(int64(r.ID), 10), row: r, cols: cols, sc: sc,
		defaults: autoactions.DefaultWaterfallsFor(cols)}
}

func toRowRec(id int32, lead pgLead, data, enr []byte) rowRec {
	r := rowRec{ID: id, Data: data, Enrichments: enr}
	if lead.Valid {
		v := int64(lead.Int32)
		r.LeadID = &v
	}
	return r
}

type pgLead struct {
	Int32 int32
	Valid bool
}

// targetRef is one explicit target of the payload.
type targetRef struct {
	workbookID string
	rowID      int32
}

// payloadTargets reads payload["targets"]: ok is false when the list is absent
// or empty (a sweep). Elements that are not objects, or whose row id is not an
// integer that fits a row id (Python crashed on those, D4), never match a row
// and are dropped; the cap applies to the raw list like targets[:max_rows].
func payloadTargets(raw any, maxRows int) (refs []targetRef, ok bool) {
	list, isList := raw.([]any)
	if !isList || len(list) == 0 {
		return nil, false
	}
	if maxRows >= 0 && len(list) > maxRows {
		list = list[:maxRows]
	}
	for _, it := range list {
		m, isMap := it.(map[string]any)
		if !isMap {
			continue
		}
		wb, _ := m["workbook_id"].(string)
		id, good := intRowID(m["row_id"])
		if !good {
			continue
		}
		refs = append(refs, targetRef{workbookID: wb, rowID: id})
	}
	return refs, true
}

// intRowID is int(row_id): a decimal string (surrounding whitespace allowed) or
// a number with its fraction truncated.
func intRowID(v any) (int32, bool) {
	var n int64
	switch x := v.(type) {
	case string:
		p, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, false
		}
		n = p
	case json.Number:
		if p, err := x.Int64(); err == nil {
			n = p
		} else if f, ferr := x.Float64(); ferr == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			n = int64(f)
		} else {
			return 0, false
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		n = int64(x)
	case int64:
		n = x
	case int:
		n = int64(x)
	default:
		return 0, false
	}
	if n < 1 || n > math.MaxInt32 {
		return 0, false
	}
	return int32(n), true
}

// resolveCandidates is resolve_candidate_rows: explicit targets (the row must
// belong to the named workbook of this workspace) or a sweep over the rule's
// scoped workbooks ordered by created_at, id, capped at maxRows in total.
func resolveCandidates(ctx context.Context, tx pgx.Tx, ws string, scope []string, targets any, maxRows int) ([]candidate, error) {
	q := gen.New(tx)
	if refs, isTargets := payloadTargets(targets, maxRows); isTargets {
		cache := map[string]struct {
			cols []autoactions.Column
			sc   []safecond.Column
		}{}
		var out []candidate
		for _, t := range refs {
			row, err := q.AutoRunRowByID(ctx, gen.AutoRunRowByIDParams{ID: t.rowID, WorkspaceID: ws})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if row.WorkbookID != t.workbookID {
				continue
			}
			c, seen := cache[t.workbookID]
			if !seen {
				raw, err := q.AutoRunWorkbookColumns(ctx, gen.AutoRunWorkbookColumnsParams{ID: t.workbookID, WorkspaceID: ws})
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return nil, err
				}
				c.cols, c.sc = parseColumns(raw)
				cache[t.workbookID] = c
			}
			var lead pgLead
			if row.LeadID.Valid {
				lead = pgLead{Int32: row.LeadID.Int32, Valid: true}
			}
			out = append(out, newCandidate(t.workbookID, toRowRec(row.ID, lead, row.Data, row.Enrichments), c.cols, c.sc))
		}
		return out, nil
	}

	wbs, err := q.AutoRunWorkbooksByIDs(ctx, gen.AutoRunWorkbooksByIDsParams{WorkspaceID: ws, Ids: nonNil(scope)})
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, wb := range wbs {
		cols, sc := parseColumns(wb.ColumnsConfig)
		rows, err := q.AutoRunSweepRows(ctx, gen.AutoRunSweepRowsParams{
			WorkbookID: wb.ID, WorkspaceID: ws, RowLimit: int32(maxRows - len(out))})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			var lead pgLead
			if row.LeadID.Valid {
				lead = pgLead{Int32: row.LeadID.Int32, Valid: true}
			}
			out = append(out, newCandidate(wb.ID, toRowRec(row.ID, lead, row.Data, row.Enrichments), cols, sc))
		}
		if len(out) >= maxRows {
			break
		}
	}
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
