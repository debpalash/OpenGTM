package workbook

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colcompute"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// runCell routes one cell by column type (enrich_cell's "Route by column type").
func (rs *runState) runCell(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (cellOutcome, error) {
	switch col.Type {
	case "formula", "http":
		return rs.computedCell(ctx, lead, col, rowID, leadID)
	case rpcAIFormula, rpcResearch, rpcAgent:
		return rs.pyCell(ctx, lead, col, rowID, leadID)
	}
	return rs.waterfall(ctx, lead, col, rowID, leadID)
}

// rowForColumn is lead_data as Python's formula/http/ai columns see it: the
// hydrated row without the contract-inputs carrier.
func rowForColumn(lead map[string]any) map[string]any {
	out := make(map[string]any, len(lead))
	for k, v := range lead {
		if k != contractInputsKey {
			out[k] = v
		}
	}
	return out
}

func (rs *runState) columnInfos() []colcompute.ColumnInfo {
	out := make([]colcompute.ColumnInfo, len(rs.cols))
	for i, c := range rs.cols {
		out[i] = colcompute.ColumnInfo{ID: c.ID, Name: c.Name}
	}
	return out
}

// emptyValue ports `value is None or value == "" or value == [] or value == {}`
// over a stored (JSON-decoded) value.
func emptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// computedCell runs a formula or http column (no provider, no spend) and
// persists its cell. The egress-guarded client is always the colcompute one: a
// row-controlled URL never travels over a caller-supplied client.
func (rs *runState) computedCell(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (cellOutcome, error) {
	row := rowForColumn(lead)
	var value any
	var errText string
	switch col.Type {
	case "formula":
		res := colcompute.ExecuteFormulaColumn(col.cfg, row, rs.columnInfos())
		value, errText = res.Value, res.Error
		if !res.Success {
			value = nil
		}
	case "http":
		hc, err := httpColumnOf(col)
		if err != nil {
			errText = "invalid_http_column"
			break
		}
		res, err := // The client argument is ignored by colcompute (always the dial-guarded
			// egress client). secrets is nil on purpose: Python's http_column has no
			// {secret:NAME} support, so such a placeholder must stay unexpanded.
			colcompute.RunHTTPColumn(ctx, nil, hc, rs.valuesForHTTP(row), nil)
		if err != nil {
			if ctx.Err() != nil {
				return cellOutcome{}, context.Cause(ctx)
			}
			return cellOutcome{}, err // Python raises ValueError out of the cell
		}
		value, errText = res.Value, res.Error
		if !res.Success {
			value = nil
		}
	}
	r := cellResult{Provider: col.Type, Err: errText}
	if value != nil {
		text, raw, err := colcompute.StoredValue(value)
		if err != nil {
			return cellOutcome{}, err
		}
		if !emptyValue(raw) {
			r.Has, r.Value, r.Raw, r.HasRaw = true, text, raw, true
			if _, isStr := value.(string); isStr {
				r.HasRaw = false
			}
		}
	}
	return rs.commitCell(ctx, col, rowID, leadID, r)
}

// valuesForHTTP is the row the http templates resolve against: the raw row
// values keyed by id (colcompute applies the name aliases itself).
func (rs *runState) valuesForHTTP(row map[string]any) map[string]any {
	return rowValuesAsCells(row, rs.columnInfos())
}

// rowValuesAsCells ports `_get_row_values({k: {"value": v}}, columns_config)`:
// the flat {id or name: text} map the {column} resolver reads.
func rowValuesAsCells(row map[string]any, cols []colcompute.ColumnInfo) map[string]any {
	cells := make(map[string]any, len(row))
	for k, v := range row {
		cells[k] = map[string]any{"value": v}
	}
	vals := colcompute.RowValues(cells, cols)
	out := make(map[string]any, len(vals))
	for k, v := range vals {
		out[k] = v
	}
	return out
}

// httpColumnOf reads the http_* config of a column.
func httpColumnOf(col column) (colcompute.HTTPColumn, error) {
	hc := colcompute.HTTPColumn{
		URL: stringOf(col.cfg["http_url"]), Method: stringOf(col.cfg["http_method"]),
		JSONPath: stringOf(col.cfg["http_extract"]), Headers: map[string]string{},
	}
	if h, ok := col.cfg["http_headers"].(map[string]any); ok {
		for k, v := range h {
			hc.Headers[k] = manifest.PyStr(v)
		}
	}
	if raw, ok := col.raw["http_body"]; ok && string(raw) != "null" {
		hc.JSONBody = raw
	}
	return hc, nil
}

func stringOf(v any) string {
	if v == nil || !manifest.PyTruthy(v) {
		return ""
	}
	return manifest.PyStr(v)
}

// rawFields decodes the top-level fields of a column config as raw JSON, so
// key order inside structured values (an http body) survives.
func rawFields(b json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func trim(s string) string { return strings.TrimSpace(s) }
