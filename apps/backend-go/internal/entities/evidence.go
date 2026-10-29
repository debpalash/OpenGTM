package entities

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// OpError is an `{"error": code}` answer of merge_entities/split_entity (HTTP 400 with detail=code).
type OpError struct{ Code string }

func (e *OpError) Error() string { return e.Code }

// Error codes, verbatim from graph.py (invalid_entity_fields is a Go-only hardening).
const (
	ErrSameEntity    = "same_entity"
	ErrEntityMissing = "entity_not_found"
	ErrCrossWS       = "cross_workspace_merge_forbidden"
	ErrLogMissing    = "merge_log_not_found_or_reverted"
	ErrBadSnapshot   = "bad_snapshot"
	ErrBadFields     = "invalid_entity_fields"
)

func opErr(code string) error { return &OpError{Code: code} }

// state is a CompanyEntity row with its JSON columns decoded in order.
type state struct {
	ID, WorkspaceID, Name string
	Domain, Phone         *string
	Email, City           *string
	IdentityKeys          any // *OMap normally
	Fields                *OMap
	Sources               []any
	Agreement             *OMap
	Corroboration         *int32
	Observation           *int32
}

func ptr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func pgText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func i32(i pgtype.Int4) *int32 {
	if !i.Valid {
		return nil
	}
	return &i.Int32
}

func pgI32(i *int32) pgtype.Int4 {
	if i == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *i, Valid: true}
}

func objOr(raw []byte) *OMap {
	if m, ok := asObject(parseOr(raw, nil)); ok {
		return m
	}
	return NewOMap()
}

func fromRow(e gen.CompanyEntity) *state {
	srcs, _ := asList(parseOr(e.Sources, nil))
	if srcs == nil {
		srcs = []any{}
	}
	var ik any = NewOMap()
	if v := parseOr(e.IdentityKeys, nil); truthy(v) {
		ik = v
	}
	return &state{
		ID: e.ID, WorkspaceID: e.WorkspaceID, Name: e.CanonicalName,
		Domain: ptr(e.PrimaryDomain), Phone: ptr(e.PrimaryPhone), Email: ptr(e.PrimaryEmail), City: ptr(e.PrimaryCity),
		IdentityKeys: ik, Fields: objOr(e.Fields), Sources: srcs, Agreement: objOr(e.SourceAgreement),
		Corroboration: i32(e.CorroborationCount), Observation: i32(e.ObservationCount),
	}
}

// api is CompanyEntity.to_api() as ordered JSON.
func (s *state) api() *OMap {
	m := NewOMap()
	m.Set("id", s.ID)
	m.Set("workspace_id", s.WorkspaceID)
	m.Set("canonical_name", s.Name)
	for _, kv := range []struct {
		k string
		v *string
	}{{"primary_domain", s.Domain}, {"primary_phone", s.Phone}, {"primary_email", s.Email}, {"primary_city", s.City}} {
		if kv.v == nil {
			m.Set(kv.k, nil)
		} else {
			m.Set(kv.k, *kv.v)
		}
	}
	m.Set("identity_keys", s.IdentityKeys)
	m.Set("fields", s.Fields)
	m.Set("sources", s.Sources)
	m.Set("source_agreement", s.Agreement)
	m.Set("corroboration_count", intOrNil(s.Corroboration))
	m.Set("observation_count", intOrNil(s.Observation))
	return m
}

func intOrNil(i *int32) any {
	if i == nil {
		return nil
	}
	return int64(*i)
}

func (s *state) save(ctx context.Context, q *gen.Queries) error {
	return q.UpdateCompanyEntityState(ctx, gen.UpdateCompanyEntityStateParams{
		ID: s.ID, WorkspaceID: s.WorkspaceID, CanonicalName: s.Name,
		PrimaryDomain: pgText(s.Domain), PrimaryPhone: pgText(s.Phone), PrimaryEmail: pgText(s.Email), PrimaryCity: pgText(s.City),
		IdentityKeys: MarshalJSON(s.IdentityKeys), Fields: MarshalJSON(s.Fields), Sources: MarshalJSON(s.Sources),
		SourceAgreement: MarshalJSON(s.Agreement), CorroborationCount: pgI32(s.Corroboration), ObservationCount: pgI32(s.Observation),
	})
}

func (s *state) lead() Lead {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return Lead{Company: s.Name, Website: str(s.Domain), Phone: str(s.Phone), Email: str(s.Email), City: str(s.City)}
}

// fieldsAreLists reports whether every provenance field holds a list: the shape the Python
// merge assumes (anything else raises TypeError/AttributeError there, a 500).
func fieldsAreLists(m *OMap) bool {
	for _, k := range m.keys {
		if _, ok := asList(m.vals[k]); !ok {
			return false
		}
	}
	return true
}

func distinctCount(sources []any) int32 {
	seen := map[string]bool{}
	for _, s := range sources {
		seen[keyOf(s)] = true
	}
	return int32(len(seen))
}

// orderedUnion is list({*a, *b}) with a deterministic (first-seen) order.
func orderedUnion(a, b []any) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, l := range [][]any{a, b} {
		for _, s := range l {
			if k := keyOf(s); !seen[k] {
				seen[k] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func strOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return string(MarshalJSON(v))
}

// recompute is graph._recompute: frequency winners -> convenience columns, agreement, corroboration.
func (s *state) recompute() {
	agreement := NewOMap()
	for _, field := range s.Fields.keys {
		obs, _ := asList(s.Fields.vals[field])
		var order []string
		counts := map[string]int{}
		values := map[string]any{}
		total := 0
		for _, o := range obs {
			om, ok := asObject(o)
			if !ok {
				continue
			}
			v, _ := om.Get("value")
			if !truthy(v) {
				continue
			}
			k := keyOf(v)
			if _, seen := counts[k]; !seen {
				order = append(order, k)
				values[k] = v
			}
			counts[k]++
			total++
		}
		if total == 0 {
			continue
		}
		best := order[0]
		for _, k := range order[1:] {
			if counts[k] > counts[best] {
				best = k
			}
		}
		ratio, _ := strconv.ParseFloat(strconv.FormatFloat(float64(counts[best])/float64(total), 'f', 3, 64), 64)
		agreement.Set(field, ratio)
		w, isStr := values[best].(string)
		if !isStr {
			continue
		}
		switch field {
		case "company":
			s.Name = w
		case "website":
			d := IdentityDomain(w)
			s.Domain = &d
		case "phone":
			s.Phone = &w
		case "email":
			s.Email = &w
		case "city":
			s.City = &w
		}
	}
	s.Agreement = agreement
	n := distinctCount(s.Sources)
	s.Corroboration = &n
}

// mergeEvidence folds merged's provenance into kept (kf[field] = kf.get(field, []) + obs).
func mergeEvidence(kept, merged *state) {
	kf := kept.Fields.Clone()
	for _, field := range merged.Fields.keys {
		cur, _ := asList(kf.vals[field])
		obs, _ := asList(merged.Fields.vals[field])
		kf.Set(field, append(append([]any{}, cur...), obs...))
	}
	kept.Fields = kf
	kept.Sources = orderedUnion(kept.Sources, merged.Sources)
	sum := int32(0)
	if kept.Observation != nil {
		sum += *kept.Observation
	}
	if merged.Observation != nil {
		sum += *merged.Observation
	}
	kept.Observation = &sum
}

func getOr(m *OMap, k string, def any) any {
	if v, ok := m.Get(k); ok {
		return v
	}
	return def
}

func listOf(v any) []any { l, _ := asList(v); return l }

// restoreEvidence is graph._restore_company_evidence: subtract this merge's contributions from
// kept, leaving later observations intact.
func restoreEvidence(kept *state, snap *OMap) {
	merged := objOrEmpty(getOr(snap, "entity", nil))
	var before *OMap
	if b, ok := asObject(getOr(snap, "kept_entity", nil)); ok && b.Len() > 0 {
		before = b
	} else {
		before = kept.api()
	}
	fields := NewOMap()
	for _, f := range kept.Fields.keys {
		fields.Set(f, append([]any{}, listOf(kept.Fields.vals[f])...))
	}
	mergedFields := objOrEmpty(getOr(merged, "fields", nil))
	for _, field := range mergedFields.keys {
		cur, _ := fields.Get(field)
		remaining := listOf(cur)
		for _, ob := range listOf(mergedFields.vals[field]) {
			for i, r := range remaining {
				if jsonEqual(ob, r) {
					remaining = append(remaining[:i:i], remaining[i+1:]...)
					break
				}
			}
		}
		if len(remaining) > 0 {
			fields.Set(field, remaining)
		} else {
			fields.Delete(field)
		}
	}
	kept.Fields = fields
	cnt := int64(0)
	if kept.Observation != nil {
		cnt = int64(*kept.Observation)
	}
	if mo, ok := asInt(getOr(merged, "observation_count", nil)); ok {
		cnt -= mo
	}
	if cnt < 0 {
		cnt = 0
	}
	c32 := int32(cnt)
	kept.Observation = &c32

	originalFields := objOrEmpty(getOr(before, "fields", nil))
	originalSources := map[string]bool{}
	for _, f := range originalFields.keys {
		for _, o := range listOf(originalFields.vals[f]) {
			if om, ok := asObject(o); ok {
				originalSources[keyOf(getOr(om, "source", nil))] = true
			}
		}
	}
	sources := []any{}
	for _, s := range listOf(getOr(before, "sources", nil)) {
		if !originalSources[keyOf(s)] {
			sources = append(sources, s)
		}
	}
	for _, f := range fields.keys {
		for _, o := range listOf(fields.vals[f]) {
			om, ok := asObject(o)
			if !ok {
				continue
			}
			src := getOr(om, "source", nil)
			if !truthy(src) {
				continue
			}
			dup := false
			for _, s := range sources {
				if jsonEqual(s, src) {
					dup = true
					break
				}
			}
			if !dup {
				sources = append(sources, src)
			}
		}
	}
	kept.Sources = sources

	// Reset convenience values supplied only by the removed company.
	reset := func(dst **string, key, field, def string) {
		if truthy(getOr(originalFields, field, nil)) {
			d := def
			*dst = &d
			return
		}
		v, present := before.Get(key)
		switch x := v.(type) {
		case string:
			*dst = &x
		case nil:
			if present {
				*dst = nil
			} else {
				d := def
				*dst = &d
			}
		default:
			d := def
			*dst = &d
		}
	}
	name := kept.Name
	var namePtr *string = &name
	reset(&namePtr, "canonical_name", "company", "(unknown)")
	if namePtr == nil {
		d := "(unknown)"
		namePtr = &d
	}
	kept.Name = *namePtr
	reset(&kept.Domain, "primary_domain", "website", "")
	reset(&kept.Phone, "primary_phone", "phone", "")
	reset(&kept.Email, "primary_email", "email", "")
	reset(&kept.City, "primary_city", "city", "")

	identity := NewOMap()
	if ik, ok := asObject(getOr(before, "identity_keys", nil)); ok {
		for _, k := range ik.keys {
			identity.Set(k, append([]any{}, listOf(ik.vals[k])...))
		}
	}
	for _, spec := range []struct {
		field, key string
		norm       func(string) string
	}{
		{"company", "name_variants", func(s string) string { return s }},
		{"website", "domains", IdentityDomain},
		{"phone", "phones", NormalizePhone},
		{"email", "emails", func(s string) string { return s }},
	} {
		valueOf := func(o any) (string, bool) {
			om, ok := asObject(o)
			if !ok {
				return "", false
			}
			v, _ := om.Get("value")
			if !truthy(v) {
				v = ""
			}
			return spec.norm(strOf(v)), true
		}
		observedBefore := map[string]bool{}
		for _, o := range listOf(getOr(originalFields, spec.field, nil)) {
			if v, ok := valueOf(o); ok {
				observedBefore[v] = true
			}
		}
		values := []any{}
		cur, _ := identity.Get(spec.key)
		for _, v := range listOf(cur) {
			if str, ok := v.(string); ok && observedBefore[str] {
				continue
			}
			values = append(values, v)
		}
		for _, o := range listOf(fields.vals[spec.field]) {
			v, ok := valueOf(o)
			if !ok || v == "" {
				continue
			}
			dup := false
			for _, e := range values {
				if e == any(v) {
					dup = true
					break
				}
			}
			if !dup {
				values = append(values, v)
			}
		}
		identity.Set(spec.key, values)
	}
	kept.IdentityKeys = identity
	kept.recompute()
}

func objOrEmpty(v any) *OMap {
	if m, ok := asObject(v); ok {
		return m
	}
	return NewOMap()
}
