package runplan

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// PayloadVersion is the "go" block version (go_executor.PAYLOAD_VERSION and
// workbook.PayloadVersion).
const PayloadVersion = 1

// kv / ordered: a JSON object whose keys keep the order Python's dicts have.
// Values are any canonjson value, nested ordered, or raw (pre-rendered JSON).
type kv struct {
	k string
	v any
}
type ordered []kv
type raw []byte

func writeValue(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case ordered:
		b.WriteByte('{')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			k, err := canonjson.Marshal(e.k, false)
			if err != nil {
				return err
			}
			b.Write(k)
			b.WriteByte(':')
			if err := writeValue(b, e.v); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case raw:
		b.Write(x)
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return writeValue(b, items)
	case []int64:
		items := make([]any, len(x))
		for i, n := range x {
			items[i] = n
		}
		return writeValue(b, items)
	case int:
		return writeValue(b, int64(x))
	default:
		out, err := canonjson.Marshal(v, true)
		if err != nil {
			return err
		}
		b.Write(out)
	}
	return nil
}

func render(v ordered) ([]byte, error) {
	var b bytes.Buffer
	if err := writeValue(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// nullable renders an optional string: Python's None is the empty value here
// (the Go manifest does not distinguish null from "").
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strs(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func sortedMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// manifestDoc is ProviderManifest.model_dump(mode="json"), fields in model
// order. Nested dicts (headers, query, body template, mappings) are emitted
// with sorted keys: the Go manifest keeps them as maps.
func manifestDoc(m manifest.Manifest) ordered {
	mappings := map[string]any{}
	keys := make([]string, 0, len(m.Response.Mappings))
	for k := range m.Response.Mappings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		mappings[k] = m.Response.Mappings[k]
	}
	return ordered{
		{"manifest_version", m.ManifestVersion},
		{"name", m.Name},
		{"display_name", m.DisplayName},
		{"author", m.Author},
		{"homepage", m.Homepage},
		{"license", m.License},
		{"tags", strs(m.Tags)},
		{"capability", m.Capability},
		{"capabilities", strs(m.Capabilities)},
		{"description", m.Description},
		{"default_confidence", m.DefaultConfidence},
		{"cost_per_lookup", m.CostPerLookup},
		{"auth", ordered{
			{"type", m.Auth.Type},
			{"param", nullable(m.Auth.Param)},
			{"value", nullable(m.Auth.Value)},
			{"env_var", nullable(m.Auth.EnvVar)},
		}},
		{"request", ordered{
			{"method", m.Request.Method},
			{"url", m.Request.URL},
			{"headers", sortedMap(m.Request.Headers)},
			{"query", sortedMap(m.Request.Query)},
			{"body_template", m.Request.BodyTemplate},
			{"timeout", m.Request.Timeout},
		}},
		{"response", ordered{
			{"error_path", nullable(m.Response.ErrorPath)},
			{"error_message_path", nullable(m.Response.ErrorMessagePath)},
			{"mappings", mappings},
		}},
		{"input_fields", strs(m.InputFields)},
	}
}

// providerNames is dict.fromkeys(n for c in columns for n in _provider_chain(c)).
func providerNames(cols []colgraph.Col) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for _, c := range cols {
		chain, err := providerChain(c)
		if err != nil {
			return nil, err
		}
		for _, n := range chain {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names, nil
}

// GoBlock is go_executor.build_go_payload: the payload's "go" block. It holds
// manifests, their sha256 (taken from the index entry, never recomputed) and
// costs, never credentials. cols are the selected columns; every provider of
// their chains must be in idx.
func GoBlock(cols []colgraph.Col, idx map[string]manifest.Entry, cost func(string) float64, defaultConcurrency int) ([]byte, error) {
	names, err := providerNames(cols)
	if err != nil {
		return nil, err
	}
	var manifests, shas, costs ordered
	for _, n := range names {
		e, ok := idx[n]
		if !ok {
			return nil, fmt.Errorf("runplan: provider %q has no manifest", n)
		}
		manifests = append(manifests, kv{n, manifestDoc(e.Manifest)})
		shas = append(shas, kv{n, e.SHA256})
		costs = append(costs, kv{n, cost(n)})
	}
	if manifests == nil { // empty dicts, not null
		manifests, shas, costs = ordered{}, ordered{}, ordered{}
	}
	return render(ordered{
		{"version", int64(PayloadVersion)},
		{"manifests", manifests},
		{"manifest_sha256", shas},
		{"costs", costs},
		{"default_concurrency", int64(defaultConcurrency)},
	})
}

// jobFields are the run_workbook job payload fields in Python's key order.
type jobFields struct {
	WorkbookID, WorkspaceID, RunID string
	ColumnIDs                      []string
	RowColumns                     ordered // nil = null
	RowIDs, LeadIDs                []int64 // nil = null
	Perf                           Perf
	FillMissing, Force             bool
}

func nullableInts(ids []int64) any {
	if ids == nil {
		return nil
	}
	return ids
}

// jobPayload renders the payload dict exactly as run_workbook builds it; goBlock,
// when set, is appended last like `payload["go"] = ...`.
func jobPayload(f jobFields, goBlock []byte) ([]byte, error) {
	var rowCols any
	if f.RowColumns != nil {
		rowCols = f.RowColumns
	}
	doc := ordered{
		{"workbook_id", f.WorkbookID},
		{"workspace_id", f.WorkspaceID},
		{"run_id", f.RunID},
		{"column_ids", f.ColumnIDs},
		{"row_columns", rowCols},
		{"row_ids", nullableInts(f.RowIDs)},
		{"lead_ids", nullableInts(f.LeadIDs)},
		{"concurrency", f.Perf.RowConcurrency},
		{"max_providers", f.Perf.MaxProviders},
		{"retry_passes", f.Perf.RetryPasses},
		{"provider_workers", f.Perf.ProviderWorkers},
		{"provider_timeout", f.Perf.ProviderTimeout},
		{"fill_missing", f.FillMissing},
		{"force", f.Force},
	}
	if goBlock != nil {
		doc = append(doc, kv{"go", raw(goBlock)})
	}
	return render(doc)
}
