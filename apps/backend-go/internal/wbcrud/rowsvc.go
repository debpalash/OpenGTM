package wbcrud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
)

// Row operations of routers/workbooks.py "Row Management (v2)". Every method
// runs inside the caller's workspace transaction with the workbook already
// loaded by Owned; nothing here commits, so an error rolls back every write
// (all-or-nothing). Row-adding paths call the automation hook on the same tx.

const insertChunk = 1000

var (
	defaultRegOnce sync.Once
	defaultReg     *wbregistry.Registry
)

// leadFieldSet is the LEAD_FIELD_MAP key set.
func (s *Service) leadFieldList() []string {
	reg := s.Reg
	if reg == nil {
		defaultRegOnce.Do(func() { defaultReg, _ = wbregistry.Load() })
		reg = defaultReg
	}
	if reg == nil {
		return nil
	}
	return reg.LeadFields()
}

// editableFields is LEAD_FIELD_MAP plus the lead_field-or-id of every
// lead_field / input column.
func (s *Service) editableFields(cols []Column) map[string]struct{} {
	allowed := map[string]struct{}{}
	for _, f := range s.leadFieldList() {
		allowed[f] = struct{}{}
	}
	for _, c := range cols {
		typ, _ := c.Get("type").(string)
		if typ != "lead_field" && typ != "input" {
			continue
		}
		v := c.Get("lead_field")
		if !truthy(v) {
			v = c.Get("id")
		}
		if k, ok := v.(string); ok {
			allowed[k] = struct{}{}
		}
	}
	return allowed
}

var protectedRowFields = map[string]struct{}{"id": {}, "row_id": {}, "lead_id": {}}

// AddRowsResult is the add_rows response (key order: added, skipped_duplicates, total_rows).
type AddRowsResult struct {
	Added             int `json:"added"`
	SkippedDuplicates int `json:"skipped_duplicates"`
	TotalRows         int `json:"total_rows"`
}

// existingIdentities streams the workbook's rows and collects their dedupe
// identities (memory is bounded by the number of distinct identities).
func existingIdentities(ctx context.Context, tx pgx.Tx, workbookID string) (map[string]struct{}, error) {
	rows, err := tx.Query(ctx, `SELECT
		CASE WHEN json_typeof(data->'website') = 'string' THEN data->>'website' END,
		CASE WHEN json_typeof(data->'domain') = 'string' THEN data->>'domain' END,
		CASE WHEN json_typeof(data->'company') = 'string' THEN data->>'company' END
		FROM workbook_rows WHERE workbook_id = $1`, workbookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var w, d, c *string
		if err := rows.Scan(&w, &d, &c); err != nil {
			return nil, err
		}
		if id := identityOf(deref(w), deref(d), deref(c)); id != "" {
			seen[id] = struct{}{}
		}
	}
	return seen, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// leadIDOf is `lead_id=row_data.get("id")`: an integer id, else NULL (Python
// sends anything else to the database and answers 500).
func leadIDOf(o *Obj) string {
	v, _ := o.Get("id")
	n, ok := v.(json.Number)
	if !ok {
		return ""
	}
	s := string(n)
	if isIntText(s) {
		if i, err := strconv.ParseInt(s, 10, 32); err == nil {
			return strconv.FormatInt(i, 10)
		}
		return ""
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == float64(int32(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return ""
}

// AddRows appends rows at max(position)+1.., skipping (when dedupe) rows whose
// identity already exists in the workbook or repeats within the request, then
// fires the row-added automation hook on the same transaction.
func (s *Service) AddRows(ctx context.Context, tx pgx.Tx, ws string, w Workbook, req AddRowsRequest) (AddRowsResult, error) {
	q := gen.New(tx)
	maxPos, err := q.MaxWorkbookRowPosition(ctx, w.ID)
	if err != nil {
		return AddRowsResult{}, err
	}
	rows, skipped := req.Rows, 0
	if req.Dedupe {
		seen, err := existingIdentities(ctx, tx, w.ID)
		if err != nil {
			return AddRowsResult{}, err
		}
		kept := make([]*Obj, 0, len(rows))
		for _, r := range rows {
			id := RowIdentity(r)
			if id != "" {
				if _, dup := seen[id]; dup {
					skipped++
					continue
				}
				seen[id] = struct{}{}
			}
			kept = append(kept, r)
		}
		rows = kept
	}
	var newIDs []int64
	for start := 0; start < len(rows); start += insertChunk {
		end := min(start+insertChunk, len(rows))
		datas := make([]string, 0, end-start)
		leadIDs := make([]string, 0, end-start)
		for _, r := range rows[start:end] {
			datas = append(datas, PyDumps(r, false))
			leadIDs = append(leadIDs, leadIDOf(r))
		}
		ids, err := q.AppendWorkbookRows(ctx, gen.AppendWorkbookRowsParams{
			WorkspaceID: ws, WorkbookID: w.ID, StartPosition: maxPos + int32(start), Datas: datas, LeadIds: leadIDs,
		})
		if err != nil {
			return AddRowsResult{}, err
		}
		if len(ids) != end-start { // the workbook is not visible in this workspace
			return AddRowsResult{}, ErrNotFound
		}
		for _, id := range ids {
			newIDs = append(newIDs, int64(id))
		}
	}
	sort.Slice(newIDs, func(i, j int) bool { return newIDs[i] < newIDs[j] })
	if len(newIDs) > 0 && s.Hook != nil {
		_, _ = s.Hook.RowsAdded(ctx, tx, ws, w.ID, newIDs) // errors are logged and swallowed by the hook
	}
	total, err := q.CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: w.ID, WorkspaceID: ws})
	if err != nil {
		return AddRowsResult{}, err
	}
	return AddRowsResult{Added: len(rows), SkippedDuplicates: skipped, TotalRows: int(total)}, nil
}

// markRowStorage is cell_scope.mark_row_storage: source_config gains
// row_storage_version 2. SQLAlchemy only writes (and stamps updated_at) when
// the value really changed.
func (s *Service) markRowStorage(ctx context.Context, tx pgx.Tx, ws string, w Workbook) error {
	nw := NewObj()
	if old, ok := w.SourceConfig.(*Obj); ok && old != nil {
		for _, k := range old.Keys() {
			v, _ := old.Get(k)
			nw.Set(k, v)
		}
	}
	nw.Set("row_storage_version", json.Number("2"))
	oldB, _ := json.Marshal(w.SourceConfig)
	newB, err := json.Marshal(nw)
	if err != nil {
		return err
	}
	if w.SourceConfig != nil && jsonEqual(oldB, newB) {
		return nil
	}
	return gen.New(tx).UpdateWorkbookSourceConfig(ctx, gen.UpdateWorkbookSourceConfigParams{
		SourceConfig: []byte(PyDumps(nw, false)), WorkbookID: w.ID, WorkspaceID: ws,
	})
}

// DeleteRows removes the listed rows of this workbook (ids of other workbooks
// are ignored) and returns how many went.
func (s *Service) DeleteRows(ctx context.Context, tx pgx.Tx, ws string, w Workbook, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	n, err := gen.New(tx).DeleteWorkbookRowsByIDs(ctx, gen.DeleteWorkbookRowsByIDsParams{WorkbookID: w.ID, RowIds: ids})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if err := s.markRowStorage(ctx, tx, ws, w); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// CountDrift means the live matching-row count differs from the reviewed one.
type CountDrift struct{ Expected, Actual int64 }

func (d *CountDrift) Error() string {
	return fmt.Sprintf("matching row count changed: expected %d, actual %d", d.Expected, d.Actual)
}

// DriftMessage is the 409 detail message of delete-query.
const DriftMessage = "Matching row count changed; review and confirm again"

// DeleteMatchingResult is {"deleted", "matched"}.
type DeleteMatchingResult struct{ Deleted, Matched int64 }

// deleteSQL deletes exactly the rows the query matches (cursor/limit excluded)
// without ever materialising their ids.
func (q RowQuery) deleteSQL() (string, []any) {
	return "DELETE FROM workbook_rows WHERE id IN (SELECT id FROM workbook_rows WHERE " +
		strings.Join(q.where[:q.whereBase()], " AND ") + ")", q.Args[:q.whereArgs]
}

// DeleteMatching deletes the rows of a saved-view/search query when the live
// count still equals the reviewed one and the confirmation matches. The count
// is re-checked against the number of rows the DELETE really removed; a
// concurrent change returns *CountDrift and the caller's rollback undoes it.
func (s *Service) DeleteMatching(ctx context.Context, tx pgx.Tx, ws string, w Workbook, req DeleteMatchingRequest) (DeleteMatchingResult, *GuardError, error) {
	var view *QueryView
	if req.ViewID != nil && *req.ViewID != "" {
		v, guard, err := LoadView(ctx, tx, w.ID, *req.ViewID)
		if guard != nil || err != nil {
			return DeleteMatchingResult{}, guard, err
		}
		view = v
	}
	search := ""
	if req.Search != nil {
		search = *req.Search
	}
	query, guard := BuildRowQuery(QueryWorkbook{ID: w.ID, Columns: w.Columns}, view, search)
	if guard != nil {
		return DeleteMatchingResult{}, guard, nil
	}
	countSQL, countArgs := query.CountSQL()
	var actual int64
	if err := tx.QueryRow(ctx, countSQL, countArgs...).Scan(&actual); err != nil {
		return DeleteMatchingResult{}, nil, err
	}
	if actual != req.ExpectedCount {
		return DeleteMatchingResult{}, nil, &CountDrift{Expected: req.ExpectedCount, Actual: actual}
	}
	required := fmt.Sprintf("DELETE %d ROWS", actual)
	if req.Confirmation != required {
		return DeleteMatchingResult{}, &GuardError{Status: 400, Detail: fmt.Sprintf("confirmation must equal %q", required)}, nil
	}
	delSQL, delArgs := query.deleteSQL()
	tag, err := tx.Exec(ctx, delSQL, delArgs...)
	if err != nil {
		return DeleteMatchingResult{}, nil, err
	}
	deleted := tag.RowsAffected()
	if deleted != actual {
		return DeleteMatchingResult{}, nil, &CountDrift{Expected: req.ExpectedCount, Actual: deleted}
	}
	if deleted > 0 {
		if err := s.markRowStorage(ctx, tx, ws, w); err != nil {
			return DeleteMatchingResult{}, nil, err
		}
	}
	return DeleteMatchingResult{Deleted: deleted, Matched: actual}, nil, nil
}

// ChangedRowFields is _changed_row_fields: the fields whose value is absent
// from data or differs under json.dumps(sort_keys=True) (so False != 0 and
// 1 != 1.0, while object key order is ignored).
func ChangedRowFields(data, fields *Obj) *Obj {
	out := NewObj()
	for _, k := range fields.Keys() {
		nv, _ := fields.Get(k)
		if ov, ok := data.Get(k); ok && PyDumps(ov, true) == PyDumps(nv, true) {
			continue
		}
		out.Set(k, nv)
	}
	return out
}

func rowDataObj(raw []byte) *Obj {
	if len(raw) > 0 {
		if doc, err := decodeDocument(raw, true); err == nil {
			if o, ok := doc.(*Obj); ok && o != nil {
				return o
			}
		}
	}
	return NewObj()
}

func mergeRowData(data, changed *Obj) *Obj {
	out := NewObj()
	for _, k := range data.Keys() {
		v, _ := data.Get(k)
		out.Set(k, v)
	}
	for _, k := range changed.Keys() {
		v, _ := changed.Get(k)
		out.Set(k, v)
	}
	return out
}

func reactiveIDs(cols []Column, changed []string) []any {
	ids := []any{}
	for _, c := range DownstreamColumns(cols, changed, ReactiveEligible) {
		if id := c.Get("id"); truthy(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// RowPatch is the outcome of PatchRow.
type RowPatch struct {
	Fields   []string // editable keys of the body, in body order
	Changed  []string // the subset whose value changed
	Reactive []any    // downstream column ids (recompute && changed only)
}

// PatchRow edits one row's snapshot data (update_row_data minus the recompute run).
func (s *Service) PatchRow(ctx context.Context, tx pgx.Tx, ws string, w Workbook, rowID int64, body *Obj, recompute bool) (RowPatch, *GuardError, error) {
	rows, err := gen.New(tx).LockWorkbookRows(ctx, gen.LockWorkbookRowsParams{WorkbookID: w.ID, RowIds: []int64{rowID}})
	if err != nil {
		return RowPatch{}, nil, err
	}
	if len(rows) == 0 {
		return RowPatch{}, &GuardError{Status: 404, Detail: "Workbook row not found"}, nil
	}
	allowed := s.editableFields(w.Columns)
	updates := NewObj()
	for _, k := range body.Keys() {
		if _, ok := allowed[k]; !ok {
			continue
		}
		if _, prot := protectedRowFields[k]; prot {
			continue
		}
		v, _ := body.Get(k)
		updates.Set(k, v)
	}
	if updates.Len() == 0 {
		return RowPatch{}, &GuardError{Status: 400, Detail: "No editable row fields supplied"}, nil
	}
	data := rowDataObj(rows[0].Data)
	changed := ChangedRowFields(data, updates)
	if changed.Len() > 0 {
		n, err := gen.New(tx).PatchWorkbookRowsData(ctx, gen.PatchWorkbookRowsDataParams{
			WorkbookID: w.ID, RowIds: []int64{rowID}, Datas: []string{PyDumps(mergeRowData(data, changed), false)},
		})
		if err != nil {
			return RowPatch{}, nil, err
		}
		if n != 1 {
			return RowPatch{}, &GuardError{Status: 404, Detail: "Workbook row not found"}, nil
		}
	}
	res := RowPatch{Fields: updates.Keys(), Changed: changed.Keys(), Reactive: []any{}}
	if recompute && changed.Len() > 0 {
		res.Reactive = reactiveIDs(w.Columns, res.Changed)
	}
	return res, nil, nil
}

// BulkPatch is the outcome of BulkPatchRows.
type BulkPatch struct {
	Updated       int
	ChangedFields []string // sorted union of really changed fields
	Reactive      []any    // unique downstream column ids, first-seen order
	RowIDs        []int64  // rows with downstream columns, update order
	RowColumns    *Obj     // str(row id) -> []any column ids, same order
}

// BulkPatchRows applies up to MaxBulkUpdates row updates atomically: duplicate
// ids are 400, any id that is not a row of this workbook fails the whole batch
// with 404, a non-editable field in any item fails it with 400, and only then
// are the changed rows written (one statement, one transaction).
func (s *Service) BulkPatchRows(ctx context.Context, tx pgx.Tx, ws string, w Workbook, updates []RowUpdate, recompute bool) (BulkPatch, *GuardError, error) {
	if len(updates) > MaxBulkUpdates {
		return BulkPatch{}, &GuardError{Status: 400, Detail: fmt.Sprintf("At most %d row updates are allowed", MaxBulkUpdates)}, nil
	}
	ids := make([]int64, len(updates))
	uniq := map[int64]struct{}{}
	for i, u := range updates {
		ids[i] = u.RowID
		uniq[u.RowID] = struct{}{}
	}
	if len(uniq) != len(ids) {
		return BulkPatch{}, &GuardError{Status: 400, Detail: "Duplicate row IDs are not allowed"}, nil
	}
	locked, err := gen.New(tx).LockWorkbookRows(ctx, gen.LockWorkbookRowsParams{WorkbookID: w.ID, RowIds: ids})
	if err != nil {
		return BulkPatch{}, nil, err
	}
	if len(locked) != len(ids) {
		return BulkPatch{}, &GuardError{Status: 404, Detail: "One or more workbook rows were not found"}, nil
	}
	byID := make(map[int64][]byte, len(locked))
	for _, r := range locked {
		byID[int64(r.ID)] = r.Data
	}
	allowed := s.editableFields(w.Columns)
	type pending struct {
		id      int64
		data    *Obj
		changed *Obj
	}
	var work []pending
	changedUnion := map[string]struct{}{}
	for _, u := range updates {
		fields := NewObj()
		ok := u.Fields.Len() > 0
		for _, k := range u.Fields.Keys() {
			_, isAllowed := allowed[k]
			_, prot := protectedRowFields[k]
			if !isAllowed || prot {
				ok = false
				break
			}
			v, _ := u.Fields.Get(k)
			fields.Set(k, v)
		}
		if !ok {
			return BulkPatch{}, &GuardError{Status: 400, Detail: fmt.Sprintf("Row %d contains non-editable fields", u.RowID)}, nil
		}
		data := rowDataObj(byID[u.RowID])
		changed := ChangedRowFields(data, fields)
		for _, k := range changed.Keys() {
			changedUnion[k] = struct{}{}
		}
		work = append(work, pending{id: u.RowID, data: data, changed: changed})
	}
	var writeIDs []int64
	var writeDatas []string
	for _, p := range work {
		if p.changed.Len() > 0 {
			writeIDs = append(writeIDs, p.id)
			writeDatas = append(writeDatas, PyDumps(mergeRowData(p.data, p.changed), false))
		}
	}
	if len(writeIDs) > 0 {
		n, err := gen.New(tx).PatchWorkbookRowsData(ctx, gen.PatchWorkbookRowsDataParams{WorkbookID: w.ID, RowIds: writeIDs, Datas: writeDatas})
		if err != nil {
			return BulkPatch{}, nil, err
		}
		if int(n) != len(writeIDs) {
			return BulkPatch{}, nil, errors.New("bulk row patch updated an unexpected number of rows")
		}
	}
	res := BulkPatch{Updated: len(updates), Reactive: []any{}, RowColumns: NewObj()}
	for k := range changedUnion {
		res.ChangedFields = append(res.ChangedFields, k)
	}
	sort.Strings(res.ChangedFields)
	if recompute {
		seen := map[any]struct{}{}
		for _, p := range work {
			if p.changed.Len() == 0 {
				continue
			}
			cols := reactiveIDs(w.Columns, p.changed.Keys())
			if len(cols) == 0 {
				continue
			}
			res.RowIDs = append(res.RowIDs, p.id)
			res.RowColumns.Set(strconv.FormatInt(p.id, 10), cols)
			for _, c := range cols {
				if _, dup := seen[c]; !dup {
					seen[c] = struct{}{}
					res.Reactive = append(res.Reactive, c)
				}
			}
		}
	}
	return res, nil, nil
}
