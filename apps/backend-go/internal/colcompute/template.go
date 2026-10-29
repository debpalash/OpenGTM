package colcompute

import (
	"fmt"
	"sort"
	"strings"
)

// ColumnInfo is the slice of a column config the {column} resolver needs. An empty Name
// means "no name" (the id is used).
type ColumnInfo struct{ ID, Name string }

// ValueError is the Python ValueError raised for ambiguous or (strict) unknown references.
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// CellText is ai_column.cell_text: only None is absent; 0 and False keep their text.
func CellText(v any) (text string) {
	if v == nil {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			text = fmt.Sprint(v)
		}
	}()
	return pyStr(norm(v))
}

// rowValuesOrdered is _get_row_values: {**aliases, **values} with Python dict ordering
// (cells visited in sorted key order, since a Go map has none).
func rowValuesOrdered(cells map[string]any, cols []ColumnInfo) []kv {
	names := make(map[string]string, len(cols))
	for _, c := range cols {
		n := c.Name
		if n == "" {
			n = c.ID
		}
		names[c.ID] = n
	}
	ids := make([]string, 0, len(cells))
	for id := range cells {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []kv
	pos := map[string]int{}
	put := func(k, v string) {
		if i, ok := pos[k]; ok {
			out[i].v = v
			return
		}
		pos[k] = len(out)
		out = append(out, kv{k, v})
	}
	text := make(map[string]string, len(ids))
	for _, id := range ids {
		var val any
		if cell, ok := cells[id].(map[string]any); ok {
			if v, present := cell["value"]; present {
				val = v
			} else {
				val = ""
			}
		} else {
			val = cells[id]
		}
		text[id] = CellText(val)
	}
	for _, id := range ids {
		name, ok := names[id]
		if !ok {
			name = id
		}
		put(name, text[id])
	}
	for _, id := range ids {
		put(id, text[id])
	}
	return out
}

// RowValues is ai_column._get_row_values: a flat {column id or name: text} map from row
// cells (each plain or {"value": ...}).
func RowValues(cells map[string]any, cols []ColumnInfo) map[string]string {
	out := map[string]string{}
	for _, e := range rowValuesOrdered(cells, cols) {
		out[e.k] = e.v.(string)
	}
	return out
}

// subPlaceholders is re.sub(r"\{([^}]+)\}", f, s).
func subPlaceholders(s string, f func(group string) (string, error)) (string, error) {
	rs := []rune(s)
	var out strings.Builder
	for i := 0; i < len(rs); {
		if rs[i] == '{' {
			if group, end, ok := matchPlaceholder(rs, i); ok {
				rep, err := f(group)
				if err != nil {
					return "", err
				}
				out.WriteString(rep)
				i = end
				continue
			}
		}
		out.WriteRune(rs[i])
		i++
	}
	return out.String(), nil
}

// ResolvePrompt is ai_column._resolve_prompt: {column} placeholders resolve by exact key,
// then case-insensitively, then with underscores read as spaces; an unresolved reference
// is "[name: not found]" (an error when strict). Ambiguity is a *ValueError.
func ResolvePrompt(template string, row map[string]any, strict bool) (string, error) {
	return subPlaceholders(template, func(group string) (string, error) {
		return resolveGroup(group, row, strict)
	})
}

// resolveGroup resolves the text between one pair of braces.
func resolveGroup(group string, row map[string]any, strict bool) (string, error) {
	key := pyStripSpace(group)
	if v, ok := row[key]; ok {
		return CellText(v), nil
	}
	lowerKey := pyLower(key)
	match := func(norm func(string) string, want string) (string, int) {
		seen := map[string]bool{}
		var only string
		for k, v := range row {
			if norm(k) == want {
				t := CellText(v)
				if !seen[t] {
					seen[t] = true
					only = t
				}
			}
		}
		return only, len(seen)
	}
	ambiguous := &ValueError{fmt.Sprintf("Ambiguous column reference: %s. Use an exact column ID.", key)}
	if t, n := match(pyLower, lowerKey); n > 1 {
		return "", ambiguous
	} else if n == 1 {
		return t, nil
	}
	spaced := func(k string) string { return strings.ReplaceAll(pyLower(k), "_", " ") }
	if t, n := match(spaced, strings.ReplaceAll(lowerKey, "_", " ")); n > 1 {
		return "", ambiguous
	} else if n == 1 {
		return t, nil
	}
	if strict {
		return "", &ValueError{fmt.Sprintf("Unknown column reference: %s. Fix the template before sending.", key)}
	}
	return "[" + key + ": not found]", nil
}

// ExecResult is {success, value, error} of an action column cell.
type ExecResult struct {
	Success bool
	Value   any
	Error   string
}

func truncChars(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ExecuteFormulaColumn is formula_column.execute_formula_column: col is the column config
// ("formula"), leadData the row's raw values keyed by column id.
func ExecuteFormulaColumn(col map[string]any, leadData map[string]any, cols []ColumnInfo) ExecResult {
	expr, _ := col["formula"].(string)
	expr = pyStripSpace(expr)
	if expr == "" {
		return ExecResult{Error: "no_formula"}
	}
	cells := make(map[string]any, len(leadData))
	for k, v := range leadData {
		cells[k] = map[string]any{"value": v}
	}
	value, err := evalFormula(expr, rowValuesOrdered(cells, cols))
	if err != nil {
		return ExecResult{Error: truncChars(err.Error(), 120)}
	}
	if value == nil || pyEq(value, "") {
		return ExecResult{Error: "empty_result"}
	}
	if b, ok := value.(bool); ok {
		if b {
			value = "true"
		} else {
			value = "false"
		}
	}
	return ExecResult{Success: true, Value: value}
}
