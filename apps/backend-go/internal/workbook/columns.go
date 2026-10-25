package workbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// column is one entry of workbooks.columns_config. cfg keeps the whole
// canonjson-decoded dict because Python hashes it into the spend contract.
type column struct {
	cfg  map[string]any
	ID   string
	Type string
	Name string
}

func parseColumns(raw []byte) ([]column, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("workbook: columns_config: %w", err)
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("workbook: columns_config is not a list")
	}
	cols := make([]column, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, errors.New("workbook: columns_config entry is not an object")
		}
		c := column{cfg: m}
		c.ID, _ = m["id"].(string)
		c.Type, _ = m["type"].(string)
		c.Name, _ = m["name"].(string)
		cols = append(cols, c)
	}
	return cols, nil
}

// str returns the string value of key, or "" when absent or not a string.
func (c column) str(key string) string {
	s, _ := c.cfg[key].(string)
	return s
}

// firstTruthy ports `a or b or c` over config keys.
func (c column) firstTruthy(keys ...string) string {
	for _, k := range keys {
		if v, ok := c.cfg[k]; ok && manifest.PyTruthy(v) {
			return manifest.PyStr(v)
		}
	}
	return ""
}

// target ports `target_field or lead_field or col_id`.
func (c column) target() string { return c.firstTruthy("target_field", "lead_field", "id") }

// explicitChain ports the explicit_selection logic: a configured waterfall
// (even empty) or a provider is an authorization boundary.
func (c column) explicitChain() (chain []string, explicit bool) {
	if w, ok := c.cfg["waterfall"]; ok && w != nil {
		if list, ok := w.([]any); ok {
			for _, e := range list {
				chain = append(chain, manifest.PyStr(e))
			}
		}
		return chain, true
	}
	if p, ok := c.cfg["provider"]; ok && manifest.PyTruthy(p) {
		return []string{manifest.PyStr(p)}, true
	}
	return nil, false
}

// enrichmentColTypes is ENRICHMENT_COL_TYPES.
var enrichmentColTypes = map[string]bool{
	"enrichment": true, "waterfall": true, "ai_formula": true, "research": true,
	"agent": true, "http": true, "formula": true, "output": true,
}

// goColumnTypes are the only column types this executor runs.
var goColumnTypes = map[string]bool{"enrichment": true, "waterfall": true}

// validateGoColumn refuses columns Python eligibility would never have routed
// here (the workbook may have been edited after enqueue).
func validateGoColumn(c column) error {
	if !goColumnTypes[c.Type] {
		return refuse("column %q type %q is not supported by the go executor", c.ID, c.Type)
	}
	if manifest.PyTruthy(c.cfg["condition"]) {
		return refuse("column %q has a condition", c.ID)
	}
	if refs := columnRefs(c); refs {
		return refuse("column %q references other columns, which the go executor does not order", c.ID)
	}
	t := c.target()
	if structuredFields[t] {
		return refuse("column %q targets structured field %q", c.ID, t)
	}
	if t == "email" {
		if v, ok := c.cfg["verify"]; !ok || manifest.PyTruthy(v) {
			return refuse("column %q needs email verification", c.ID)
		}
	}
	return nil
}

// checkProviders refuses a column whose provider chain names a provider the
// payload carries no manifest for (a stale or edited workbook).
func checkProviders(c column, p *runPayload) error {
	chain, explicit := c.explicitChain()
	if !explicit {
		chain = defaultWaterfalls[c.target()]
	}
	for _, name := range chain {
		if _, ok := p.Manifests[name]; !ok {
			return refuse("unknown_provider: column %q names %q, which the payload has no manifest for", c.ID, name)
		}
	}
	return nil
}

var refRE = regexp.MustCompile(`\{([^}]+)\}`)

// columnRefs reports whether the column declares {col} references or
// input_columns (column_deps._refs_in): dependency ordering, upstream-failure
// propagation and cycle detection are not ported, so such columns stay on Python.
func columnRefs(c column) bool {
	for _, f := range []string{"prompt", "formula", "http_url", "goal"} {
		if s, ok := c.cfg[f].(string); ok && refRE.MatchString(s) {
			return true
		}
	}
	if list, ok := c.cfg["input_columns"].([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok && manifest.PyStrip(s) != "" {
				return true
			}
		}
	}
	return false
}

// contractInputsKey carries the raw row's contract inputs inside the hydrated
// row map (Python: enrichment.CONTRACT_INPUTS_KEY).
const contractInputsKey = "__contract_inputs"

func jsonObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return m, nil
}

// rowExecutionData ports cell_scope.row_execution_data.
func rowExecutionData(rowID int64, leadID *int64, dataRaw, enrRaw []byte, columns []column) (map[string]any, error) {
	data, err := jsonObject(dataRaw)
	if err != nil {
		return nil, fmt.Errorf("workbook: row %d data: %w", rowID, err)
	}
	enr, err := jsonObject(enrRaw)
	if err != nil {
		return nil, fmt.Errorf("workbook: row %d enrichments: %w", rowID, err)
	}
	// The spend contract hashes only these raw stored fields: never anything an
	// enrichment column of this job wrote (a column id/name may equal a lead field).
	baseInputs := map[string]any{}
	for k, v := range leadStrings(data) {
		baseInputs[k] = v
	}
	input := make(map[string]any, len(data))
	for k, v := range data {
		input[k] = v
	}
	values, aliases := map[string]any{}, map[string]any{}
	for _, c := range columns {
		if c.Type == "lead_field" || c.Type == "input" {
			field := c.firstTruthy("lead_field", "id")
			if v, ok := input[field]; ok {
				values[c.ID] = v
				if c.Name != "" {
					aliases[c.Name] = v
				}
			}
			continue
		}
		cell, isDict := enr[c.ID].(map[string]any)
		if isDict && (cell["status"] != "complete" || cell["value"] == nil) {
			delete(data, c.ID)
			if c.Name != "" {
				delete(data, c.Name)
			}
		}
		if isDict && cell["status"] == "complete" && cell["value"] != nil {
			values[c.ID] = cell["value"]
			if c.Name != "" {
				aliases[c.Name] = cell["value"]
			}
		}
	}
	out := make(map[string]any, len(data)+len(aliases)+len(values)+3)
	for _, m := range []map[string]any{data, aliases, values} {
		for k, v := range m {
			out[k] = v
		}
	}
	var lid any
	id := rowID
	if leadID != nil {
		lid = *leadID
		if *leadID != 0 {
			id = *leadID
		}
	}
	out["id"], out["__row_id"], out["__lead_id"] = id, rowID, lid
	out[contractInputsKey] = baseInputs
	return out, nil
}

// leadStrings extracts the Lead fields the declarative templates can read.
// Lead.from_dict keeps values as-is and the compiler does `x or ""`.
func leadStrings(row map[string]any) map[string]string {
	out := make(map[string]string, 7)
	for _, k := range []string{"company", "website", "email", "linkedin_url", "city", "contact_person", "phone"} {
		if v, ok := row[k]; ok && manifest.PyTruthy(v) {
			out[k] = manifest.PyStr(v)
		}
	}
	return out
}

// intOf returns row["id"] style integer identities.
func intOf(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case json.Number:
		n, _ := strconv.ParseInt(string(x), 10, 64)
		return n
	}
	return 0
}
