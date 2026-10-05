package enrich

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Defaults and bounds. DefaultConcurrency and the provider timeout default are
// the Python ones (WORKBOOK_RUN_CONCURRENCY, WORKBOOK_PROVIDER_TIMEOUT); the
// upper bounds keep one job from claiming unbounded goroutines.
const (
	DefaultConcurrency     = 12
	MaxConcurrency         = 64
	DefaultProviderWorkers = 8
	MaxProviderWorkers     = 64
	DefaultProviderTimeout = 10.0
	MaxProviderTimeout     = 120.0
	MaxRetryPasses         = 5
)

// payload is the run_workbook job payload (see routers/workbooks.py run_workbook).
// A nil slice is Python's None ("all"); an empty non-nil slice means "none".
type payload struct {
	WorkspaceID     string
	WorkbookID      string
	ColumnIDs       []string
	RowIDs          []int64
	LeadIDs         []int64
	RowColumns      map[int64][]string // nil when absent
	Concurrency     int
	MaxProviders    int
	RetryPasses     int
	ProviderTimeout float64
	ProviderWorkers int
	FillMissing     bool
	Force           bool
}

var errBadPayload = errors.New("run_workbook_connector payload must be a JSON object with workspace_id and workbook_id")

func parsePayload(raw []byte) (payload, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return payload{}, errBadPayload
	}
	p := payload{
		Concurrency: DefaultConcurrency, RetryPasses: 1, ProviderTimeout: DefaultProviderTimeout,
		ProviderWorkers: DefaultProviderWorkers,
	}
	str := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	p.WorkspaceID, p.WorkbookID = str("workspace_id"), str("workbook_id")
	if p.WorkspaceID == "" || p.WorkbookID == "" {
		return payload{}, errBadPayload
	}
	var err error
	if p.ColumnIDs, err = stringList(m, "column_ids"); err != nil {
		return payload{}, err
	}
	if p.RowIDs, err = intList(m, "row_ids"); err != nil {
		return payload{}, err
	}
	if p.LeadIDs, err = intList(m, "lead_ids"); err != nil {
		return payload{}, err
	}
	if rc, ok := m["row_columns"]; ok && rc != nil {
		if p.RowColumns, err = rowColumns(rc); err != nil {
			return payload{}, err
		}
	}
	p.Concurrency = clampInt(m, "concurrency", DefaultConcurrency, 1, MaxConcurrency)
	p.MaxProviders = clampInt(m, "max_providers", 0, 0, 1<<20)
	p.RetryPasses = clampInt(m, "retry_passes", 1, 0, MaxRetryPasses)
	p.ProviderWorkers = clampInt(m, "provider_workers", DefaultProviderWorkers, 1, MaxProviderWorkers)
	if v, ok := number(m["provider_timeout"]); ok && v > 0 {
		p.ProviderTimeout = min(v, MaxProviderTimeout)
	}
	p.FillMissing, _ = m["fill_missing"].(bool)
	p.Force, _ = m["force"].(bool)
	return p, nil
}

func number(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	}
	return 0, false
}

func clampInt(m map[string]any, key string, def, lo, hi int) int {
	v, ok := number(m[key])
	if !ok {
		return def
	}
	return int(min(max(v, float64(lo)), float64(hi)))
}

func stringList(m map[string]any, key string) ([]string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("payload %s must be a list", key)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("payload %s must contain strings", key)
		}
		out = append(out, s)
	}
	return out, nil
}

func intList(m map[string]any, key string) ([]int64, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("payload %s must be a list", key)
	}
	out := make([]int64, 0, len(list))
	for _, e := range list {
		n, ok := e.(json.Number)
		if !ok {
			return nil, fmt.Errorf("payload %s must contain integers", key)
		}
		i, err := n.Int64()
		if err != nil {
			return nil, fmt.Errorf("payload %s must contain integers", key)
		}
		out = append(out, i)
	}
	return out, nil
}

// rowColumns validates the per-row column scope exactly as
// cell_scope.restrict_work_items does: malformed durable payloads are rejected
// rather than read as "everything".
func rowColumns(v any) (map[int64][]string, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("Invalid per-row column scope")
	}
	out := make(map[int64][]string, len(obj))
	for key, ids := range obj {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != key {
			return nil, errors.New("Invalid row identity in cell scope")
		}
		list, ok := ids.([]any)
		if !ok {
			return nil, errors.New("Invalid column identities in cell scope")
		}
		cols := make([]string, 0, len(list))
		for _, e := range list {
			s, ok := e.(string)
			if !ok || s == "" {
				return nil, errors.New("Invalid column identities in cell scope")
			}
			cols = append(cols, s)
		}
		out[id] = cols
	}
	return out, nil
}
