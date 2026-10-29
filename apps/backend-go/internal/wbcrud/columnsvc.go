package wbcrud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Column routes (routers/workbooks.py update_column_settings, update_column_order,
// update_column_width, add_column, remove_column). Every method runs one
// workspace transaction that locks the workbook FOR UPDATE, so concurrent edits
// serialize and the loser sees the winner's state. Failures that FastAPI answers
// as HTTPException come back as *GuardError.

// settingsAllowed are the ColumnConfig fields PATCH .../settings may change
// (every field except id, type and width).
var settingsAllowed = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range columnOrder {
		m[k] = true
	}
	delete(m, "id")
	delete(m, "type")
	delete(m, "width")
	return m
}()

func guard(status int, detail string) *GuardError { return &GuardError{Status: status, Detail: detail} }

// lock loads the workspace's workbook FOR UPDATE; a missing or foreign id is
// the 404 guard.
func (s *Service) lock(ctx context.Context, tx pgx.Tx, ws, id string) (Workbook, error) {
	w, err := s.Owned(ctx, tx, ws, id, true)
	if errors.Is(err, ErrNotFound) {
		return w, guard(404, NotFoundDetail)
	}
	return w, err
}

func hasID(c Column, id string) bool {
	s, ok := c.Get("id").(string)
	return ok && s == id
}

// findColumn returns the indexes of columns whose id equals id.
func findColumn(cols []Column, id string) []int {
	var idx []int
	for i, c := range cols {
		if hasID(c, id) {
			idx = append(idx, i)
		}
	}
	return idx
}

func cloneAll(cols []Column) []Column {
	out := make([]Column, len(cols))
	for i, c := range cols {
		out[i] = c.Clone()
	}
	return out
}

// UpdateColumnSettings applies a compare-and-set patch of presentation-neutral
// settings to one column and returns the changes as validated (strict mode).
func (s *Service) UpdateColumnSettings(ctx context.Context, ws, wbID, colID string, b SettingsBody) (*Obj, error) {
	keys := b.Changes.Keys()
	ek := b.Expected.Keys()
	same := len(keys) == len(ek)
	for _, k := range keys {
		if _, ok := b.Expected.Get(k); !ok {
			same = false
		}
		if !settingsAllowed[k] {
			same = false
		}
	}
	if len(keys) == 0 || !same {
		return nil, guard(422, "Provide supported changes and matching expected fields")
	}
	var out *Obj
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		w, err := s.lock(ctx, tx, ws, wbID)
		if err != nil {
			return err
		}
		idx := findColumn(w.Columns, colID)
		if len(idx) == 0 {
			return guard(404, "Column not found")
		}
		if len(idx) != 1 {
			return guard(409, "Column identity is ambiguous")
		}
		current := w.Columns[idx[0]]
		for _, k := range ek {
			want, _ := b.Expected.Get(k)
			if DumpsSorted(current.Get(k)) != DumpsSorted(want) {
				return guard(409, "Column settings changed. Refresh before saving again.")
			}
		}
		// {"name": column_id, **current, **changes}
		cand := NewObj()
		cand.Set("name", colID)
		for _, k := range current.Keys() {
			cand.Set(k, current.Get(k))
		}
		for _, k := range keys {
			v, _ := b.Changes.Get(k)
			cand.Set(k, v)
		}
		validated, ok := ValidateColumnStrict(cand)
		if !ok {
			// Never reflect prompts, provider secrets or destination configuration.
			return guard(422, "Invalid column settings")
		}
		if _, changesName := b.Changes.Get("name"); changesName {
			if n, _ := validated.Get("name"); IsBlank(n.(string)) {
				return guard(422, "Column name must not be empty")
			}
		}
		changes := NewObj()
		for _, k := range keys {
			v, _ := validated.Get(k)
			changes.Set(k, v)
		}
		replacement := make([]Column, len(w.Columns))
		for i, c := range w.Columns {
			if hasID(c, colID) {
				replacement[i] = MergeChanges(c, changes)
			} else {
				replacement[i] = c.Clone()
			}
		}
		if g := ValidateRenames(w.Columns, replacement); g != nil {
			return g
		}
		if g := ValidateNewCycles(w.Columns, replacement); g != nil {
			return g
		}
		if g := ValidateOutputColumns(w.Columns, replacement); g != nil {
			return g
		}
		if _, _, err := s.UpdateIfChanged(ctx, tx, ws, w, UpdatePatch{Columns: &replacement}); err != nil {
			return err
		}
		out = changes
		return nil
	})
	return out, err
}

// ReorderColumns reorders the columns when the caller still sees the current order.
func (s *Service) ReorderColumns(ctx context.Context, ws, wbID string, b OrderBody) error {
	return s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		w, err := s.lock(ctx, tx, ws, wbID)
		if err != nil {
			return err
		}
		cur := make([]string, len(w.Columns))
		curOK := true
		for i, c := range w.Columns {
			id, ok := c.Get("id").(string)
			curOK = curOK && ok
			cur[i] = id
		}
		if !curOK || !equalStrings(cur, b.Expected) {
			return guard(409, "Column order changed. Refresh before reordering.")
		}
		have := map[string]Column{}
		for i, id := range cur {
			if _, dup := have[id]; dup {
				return guard(422, "Column order must contain each current column exactly once")
			}
			have[id] = w.Columns[i]
		}
		seen := map[string]bool{}
		for _, id := range b.ColumnIDs {
			if _, ok := have[id]; !ok || seen[id] {
				return guard(422, "Column order must contain each current column exactly once")
			}
			seen[id] = true
		}
		if len(b.ColumnIDs) != len(cur) {
			return guard(422, "Column order must contain each current column exactly once")
		}
		out := make([]Column, len(b.ColumnIDs))
		for i, id := range b.ColumnIDs {
			out[i] = have[id].Clone()
		}
		_, _, err = s.UpdateIfChanged(ctx, tx, ws, w, UpdatePatch{Columns: &out})
		return err
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SetColumnWidth sets the presentation width of every column with that id.
func (s *Service) SetColumnWidth(ctx context.Context, ws, wbID, colID string, width int64) error {
	return s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		w, err := s.lock(ctx, tx, ws, wbID)
		if err != nil {
			return err
		}
		if len(findColumn(w.Columns, colID)) == 0 {
			return guard(404, "Column not found")
		}
		out := cloneAll(w.Columns)
		for i := range out {
			if hasID(out[i], colID) {
				out[i].Set("width", jsonInt(width))
			}
		}
		_, _, err = s.UpdateIfChanged(ctx, tx, ws, w, UpdatePatch{Columns: &out})
		return err
	})
}

// AddColumn appends a normalised column (ParseAddBody) to the latest configuration.
func (s *Service) AddColumn(ctx context.Context, ws, wbID string, col Column) (Workbook, error) {
	var out Workbook
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		w, err := s.lock(ctx, tx, ws, wbID)
		if err != nil {
			return err
		}
		id, _ := col.Get("id").(string)
		name, _ := col.Get("name").(string)
		width, werr := strconv.ParseInt(numberText(col.Get("width")), 10, 64)
		if IsBlank(id) || IsBlank(name) || werr != nil || width < 80 || width > 600 {
			return guard(422, "Column requires a name, identity and width from 80 to 600")
		}
		if len(findColumn(w.Columns, id)) > 0 {
			return guard(409, "Column identity already exists. Refresh before adding another column.")
		}
		if len(w.Columns) >= CurrentLimits().MaxColumns {
			return guard(422, fmt.Sprintf("Workbook is limited to %d columns", CurrentLimits().MaxColumns))
		}
		cols := append(cloneAll(w.Columns), col)
		if g := ValidateNewCycles(w.Columns, cols); g != nil {
			return g
		}
		if g := ValidateOutputColumns(w.Columns, cols); g != nil {
			return g
		}
		out, _, err = s.UpdateIfChanged(ctx, tx, ws, w, UpdatePatch{Columns: &cols})
		if err != nil {
			return err
		}
		s.Hydrate(ctx, tx, ws, &out, false)
		return nil
	})
	return out, err
}

// RemoveColumn deletes one column and its legacy enrichment overlay rows. expected
// (nil = no confirmation body) must equal the stored column. Row data and
// workbook_rows.enrichments are retained, as in Python.
func (s *Service) RemoveColumn(ctx context.Context, ws, wbID, colID string, expected *Obj) (Workbook, error) {
	var out Workbook
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		w, err := s.lock(ctx, tx, ws, wbID)
		if err != nil {
			return err
		}
		idx := findColumn(w.Columns, colID)
		if len(idx) == 0 {
			return guard(404, "Column not found")
		}
		if len(idx) != 1 {
			return guard(409, "Column identity is ambiguous")
		}
		target := w.Columns[idx[0]]
		if expected != nil && DumpsSorted(target) != DumpsSorted(expected) {
			return guard(409, "Column changed since confirmation. Refresh and review before deleting.")
		}
		if deps := ReferencingColumns(w.Columns, target); len(deps) > 0 {
			names := make([]string, len(deps))
			for i, c := range deps {
				if truthy(c.Get("name")) {
					names[i] = pyStr(c.Get("name"))
				} else {
					names[i] = pyStr(c.Get("id"))
				}
			}
			return guard(409, "Column is referenced by: "+strings.Join(names, ", ")+". Update those references before deleting.")
		}
		q := gen.New(tx)
		views, err := q.ListWorkbookViewsForColumnGuard(ctx, gen.ListWorkbookViewsForColumnGuardParams{WorkbookID: wbID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		var viewNames []string
		for _, v := range views {
			if viewReferences(v.Config, colID) {
				viewNames = append(viewNames, v.Name)
			}
		}
		if len(viewNames) > 0 {
			return guard(409, "Column is referenced by saved views: "+strings.Join(viewNames, ", ")+". Update those views before deleting.")
		}
		kept := make([]Column, 0, len(w.Columns))
		for _, c := range w.Columns {
			if !hasID(c, colID) {
				kept = append(kept, c.Clone())
			}
		}
		out, _, err = s.UpdateIfChanged(ctx, tx, ws, w, UpdatePatch{Columns: &kept})
		if err != nil {
			return err
		}
		if err := q.DeleteWorkbookEnrichmentsForColumn(ctx, gen.DeleteWorkbookEnrichmentsForColumnParams{WorkbookID: wbID, WorkspaceID: ws, ColumnID: colID}); err != nil {
			return err
		}
		s.Hydrate(ctx, tx, ws, &out, false)
		return nil
	})
	return out, err
}

// viewReferences reports whether a saved view config (filters/sort rules'
// "column" or "hidden_columns") names column id. Python raised on malformed
// configs (null lists, non-object rules, a scalar config); those yield no
// reference here instead of a 500.
func viewReferences(raw []byte, id string) bool {
	if len(raw) == 0 {
		return false
	}
	doc, err := decodeDocument(raw, false)
	if err != nil {
		return false
	}
	cfg, ok := doc.(*Obj)
	if !ok {
		return false
	}
	for _, key := range []string{"filters", "sort"} {
		rules, _ := cfg.Get(key)
		list, _ := rules.([]any)
		for _, r := range list {
			if rule, ok := r.(*Obj); ok {
				if c, _ := rule.Get("column"); c == id {
					return true
				}
			}
		}
	}
	hidden, _ := cfg.Get("hidden_columns")
	list, _ := hidden.([]any)
	for _, h := range list {
		if h == id {
			return true
		}
	}
	return false
}

func jsonInt(n int64) json.Number { return json.Number(strconv.FormatInt(n, 10)) }

func numberText(v any) string {
	if n, ok := v.(json.Number); ok {
		return string(n)
	}
	return ""
}
