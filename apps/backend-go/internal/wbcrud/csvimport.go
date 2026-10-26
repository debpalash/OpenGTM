package wbcrud

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Port of prepare_csv_import / analyze_csv_import (services/workbook/csv_import.py).

// CSVMapping maps source headers to target names; a nil value is an explicit
// null (skip the source column).
type CSVMapping map[string]*string

// CSVPlanner holds the LEAD_FIELD_MAP key set.
type CSVPlanner struct{ lead map[string]struct{} }

func NewCSVPlanner(leadFields []string) *CSVPlanner {
	p := &CSVPlanner{lead: make(map[string]struct{}, len(leadFields))}
	for _, f := range leadFields {
		p.lead[f] = struct{}{}
	}
	return p
}

func (p *CSVPlanner) isLead(k string) bool { _, ok := p.lead[k]; return ok }

// InferTarget is infer_target: a canonical lead field or a stable custom key.
func (p *CSVPlanner) InferTarget(header string) string {
	normalized := NormalizeHeader(header)
	if t, ok := csvHeaderAliases[normalized]; ok {
		return t
	}
	if u := strings.ReplaceAll(normalized, " ", "_"); p.isLead(u) {
		return u
	}
	return ColumnKey(header)
}

// CSVPrepared is prepare_csv_import's result. Headers keeps first-seen order;
// Resolved holds a (possibly nil) target for every header.
type CSVPrepared struct {
	Rows     []*Obj
	Columns  []Column
	Added    []Column
	Headers  []string
	Resolved map[string]*string
}

// ResolvedObj is the resolved mapping as an ordered object.
func (c CSVPrepared) ResolvedObj() *Obj {
	o := NewObj()
	for _, h := range c.Headers {
		if t := c.Resolved[h]; t != nil {
			o.Set(h, *t)
		} else {
			o.Set(h, nil)
		}
	}
	return o
}

// csvHeaders is _headers: stripped, non-empty, first-seen order.
func csvHeaders(rows []*Obj) []string {
	var ordered []string
	seen := map[string]struct{}{}
	for _, row := range rows {
		for _, raw := range row.Keys() {
			h := pyTrim(raw)
			if h == "" {
				continue
			}
			if _, dup := seen[h]; !dup {
				seen[h] = struct{}{}
				ordered = append(ordered, h)
			}
		}
	}
	return ordered
}

func csvEmpty(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

func strPtr(s string) *string { return &s }

func firstRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// suffixPicker finds the first base_N (N >= 2) outside a growing set. Because
// the set only grows, the last N tried for a base is a valid starting point,
// which keeps thousands of colliding headers linear (Python rescans from 2).
type suffixPicker map[string]int

func (sp suffixPicker) next(base string, taken map[string]struct{}) string {
	n := sp[base]
	if n < 2 {
		n = 2
	}
	for {
		cand := base + "_" + strconv.Itoa(n)
		if _, used := taken[cand]; !used {
			sp[base] = n
			return cand
		}
		n++
	}
}

// Prepare normalises rows and extends the workbook columns without losing CSV
// fields. cols is not modified.
func (p *CSVPlanner) Prepare(rows []*Obj, cols []Column, mapping CSVMapping, create bool) CSVPrepared {
	columns := make([]Column, len(cols))
	for i, c := range cols {
		columns[i] = c.Clone()
	}
	var added []Column

	existingByName := map[string]*Column{}
	existingByExact := map[string]*Column{}
	usedIDs := map[string]struct{}{}
	usedFields := map[string]struct{}{}
	for i := range columns {
		col := &columns[i]
		cid := ""
		if v := col.Get("id"); truthy(v) {
			cid = pyStr(v)
		}
		field := cid
		if v := col.Get("lead_field"); truthy(v) {
			field = pyStr(v)
		}
		if cid != "" {
			usedIDs[cid] = struct{}{}
		}
		if t, _ := col.Get("type").(string); t == "lead_field" || t == "input" {
			setDefault := func(m map[string]*Column, k string) {
				if _, ok := m[k]; !ok {
					m[k] = col
				}
			}
			if cid != "" {
				setDefault(existingByName, NormalizeHeader(cid))
			}
			if field != "" {
				usedFields[field] = struct{}{}
				setDefault(existingByName, NormalizeHeader(field))
			}
			if name := col.Get("name"); truthy(name) {
				setDefault(existingByName, NormalizeHeader(pyStr(name)))
				setDefault(existingByExact, pyCasefold(pyTrim(pyStr(name))))
			}
		}
	}

	headers := csvHeaders(rows)
	resolved := make(map[string]*string, len(headers))
	assigned := map[string]string{}
	idPick, fieldPick := suffixPicker{}, suffixPicker{}
	for _, header := range headers {
		req, explicit := mapping[header]
		requested := ""
		if explicit && req != nil {
			requested = pyTrim(*req)
		}
		if explicit && requested == "" {
			resolved[header] = nil
			continue
		}
		targetName := requested
		if targetName == "" {
			targetName = header
		}
		target := p.InferTarget(targetName)
		keepCustom := explicit && requested == header && !p.isLead(target)
		fold := pyCasefold(targetName)
		if !keepCustom {
			if t, ok := assigned[fold]; ok {
				resolved[header] = strPtr(t)
				continue
			}
		}
		existing := existingByExact[fold]
		if existing == nil && !keepCustom {
			existing = existingByName[NormalizeHeader(targetName)]
		}
		if existing != nil {
			t := "" // `existing["id"]` raises KeyError in Python when absent
			if v := existing.Get("lead_field"); truthy(v) {
				t = pyStr(v)
			} else if id, ok := existing.Lookup("id"); ok {
				t = pyStr(id)
			}
			resolved[header] = strPtr(t)
			continue
		}

		if !p.isLead(target) && !(explicit && requested == target && !keepCustom) {
			base := target
			if _, taken := usedFields[target]; taken {
				target = fieldPick.next(base, usedFields)
			}
		}
		resolved[header] = strPtr(target)
		assigned[fold] = target
		if _, taken := usedFields[target]; !create || taken {
			usedFields[target] = struct{}{}
			continue
		}

		colID := target
		if _, taken := usedIDs[colID]; taken {
			colID = idPick.next(target, usedIDs)
		}
		display := header
		if requested != "" && p.isLead(target) {
			display = pyTitle(strings.ReplaceAll(requested, "_", " "))
		}
		display = firstRunes(display, 255)
		nc := MakeColumn("id", colID, "name", display, "type", "lead_field", "lead_field", target, "width", json.Number("200"))
		columns = append(columns, nc)
		added = append(added, nc)
		usedIDs[colID] = struct{}{}
		usedFields[target] = struct{}{}
		existingByExact[pyCasefold(display)] = &columns[len(columns)-1]
	}

	var out []*Obj
	for _, src := range rows {
		norm := NewObj()
		for _, raw := range src.Keys() {
			v, _ := src.Get(raw)
			t := resolved[pyTrim(raw)]
			if t == nil || *t == "" {
				continue
			}
			if cur, present := norm.Get(*t); !present || csvEmpty(cur) {
				norm.Set(*t, v)
			}
		}
		for _, k := range norm.Keys() {
			if v, _ := norm.Get(k); !csvEmpty(v) {
				out = append(out, norm)
				break
			}
		}
	}
	return CSVPrepared{Rows: out, Columns: columns, Added: added, Headers: headers, Resolved: resolved}
}

var csvClayMarkers = []string{"claygent", "clay url", "last enrichment date", "enrichment status"}

// Analyze is analyze_csv_import over an already prepared import: the
// deterministic migration report, keys in Python's order.
func (p *CSVPlanner) Analyze(rows []*Obj, prep CSVPrepared, fileName, sourceSystem string) *Obj {
	targets := map[string][]string{}
	for _, h := range prep.Headers {
		if t := prep.Resolved[h]; t != nil && *t != "" {
			targets[*t] = append(targets[*t], h)
		}
	}
	names := make([]string, 0, len(targets))
	for t := range targets {
		names = append(names, t)
	}
	sort.Strings(names)
	collisions := []any{}
	for _, t := range names {
		if hs := targets[t]; len(hs) > 1 {
			o := NewObj()
			o.Set("target", t)
			o.Set("headers", strSlice(hs))
			collisions = append(collisions, o)
		}
	}
	norm := map[string]struct{}{}
	for _, h := range prep.Headers {
		norm[NormalizeHeader(h)] = struct{}{}
	}
	detected, reason := sourceSystem, "explicit"
	if sourceSystem == "auto" {
		filenameMatch := strings.Contains(NormalizeHeader(fileName), "clay")
		marker := false
		for _, m := range csvClayMarkers {
			if _, ok := norm[m]; ok {
				marker = true
			}
		}
		detected = "generic"
		if filenameMatch || marker {
			detected = "clay"
		}
		switch {
		case filenameMatch:
			reason = "filename"
		case marker:
			reason = "headers"
		default:
			reason = "none"
		}
	}
	var standard, custom, skipped []string
	for _, h := range prep.Headers {
		t := prep.Resolved[h]
		switch {
		case t == nil || *t == "":
			skipped = append(skipped, h)
		case p.isLead(*t):
			standard = append(standard, h)
		default:
			custom = append(custom, h)
		}
	}
	sort.Strings(standard)
	sort.Strings(custom)
	sort.Strings(skipped)
	created := make([]string, 0, len(prep.Added))
	for _, c := range prep.Added {
		created = append(created, pyStr(c.Get("name")))
	}
	a := NewObj()
	a.Set("source_system", detected)
	a.Set("detection_reason", reason)
	a.Set("input_rows", len(rows))
	a.Set("importable_rows", len(prep.Rows))
	a.Set("headers", strSlice(prep.Headers))
	a.Set("mapped_standard", strSlice(standard))
	a.Set("custom_columns", strSlice(custom))
	a.Set("columns_created", strSlice(created))
	a.Set("skipped_columns", strSlice(skipped))
	a.Set("collisions", collisions)
	a.Set("mapping", prep.ResolvedObj())
	return a
}

func strSlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
