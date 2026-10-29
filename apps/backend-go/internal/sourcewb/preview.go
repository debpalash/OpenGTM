package sourcewb

import (
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcecatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// BuildQuery is source_engine.build_query: an ICP config as the natural-language
// collection query. The free-text description wins; otherwise industry, keywords
// and "in <geo>" are joined. A non-object ICP yields "" (Python raises).
func BuildQuery(icp any) string {
	m, _ := plain(icp).(map[string]any)
	if m == nil {
		return ""
	}
	if d := strip(orStr(m["description"])); d != "" {
		return d
	}
	var parts []string
	if truthy(m["industry"]) {
		parts = append(parts, pyStr(m["industry"]))
	}
	join := func(v any, sep string) string {
		var s []string
		for _, e := range iterate(v) {
			s = append(s, pyStr(e))
		}
		return strings.Join(s, sep)
	}
	if truthy(m["keywords_any"]) {
		parts = append(parts, join(m["keywords_any"], " "))
	}
	if truthy(m["geo"]) {
		parts = append(parts, "in "+join(m["geo"], ", "))
	}
	return strip(strings.Join(parts, " "))
}

type catalogSource struct {
	name, label, category string
	region                []any
	enabled               bool
}

// registrySources is SOURCES in priority order (sourcecatalog's get_source_summary dump).
func registrySources() []catalogSource {
	var out []catalogSource
	list, _ := sourcecatalog.Registry().Get("sources")
	items, _ := list.([]any)
	for _, it := range items {
		o, ok := asObj(it)
		if !ok {
			continue
		}
		s := catalogSource{name: pyStr(get(o, "name")), label: pyStr(get(o, "label")), category: pyStr(get(o, "category")), enabled: true}
		if r, ok := get(o, "region").([]any); ok {
			s.region = r
		}
		if e, ok := get(o, "enabled").(bool); ok {
			s.enabled = e
		}
		out = append(out, s)
	}
	return out
}

func filterValues(v any) []any {
	if !truthy(v) {
		return []any{nil}
	}
	return iterate(v)
}

// PreviewSource is source_engine.preview_source: the query that would run and the
// registry sources the channels select (enabled sources per region x category, plus
// explicit sources whatever their state), sorted by name.
func PreviewSource(icp, channels any) *wbcrud.Obj {
	ch, _ := plain(channels).(map[string]any)
	if ch == nil {
		ch = map[string]any{}
	}
	all := registrySources()
	matched := map[string]catalogSource{}
	for _, region := range filterValues(ch["regions"]) {
		for _, category := range filterValues(ch["categories"]) {
			for _, s := range all {
				if !s.enabled {
					continue
				}
				if truthy(region) {
					found := false
					for _, r := range s.region {
						if r == region {
							found = true
							break
						}
					}
					if !found {
						continue
					}
				}
				if truthy(category) && s.category != category {
					continue
				}
				matched[s.name] = s
			}
		}
	}
	if truthy(ch["explicit_sources"]) {
		for _, n := range iterate(ch["explicit_sources"]) {
			name, ok := n.(string)
			if !ok {
				continue
			}
			for _, s := range all {
				if s.name == name {
					matched[name] = s
					break
				}
			}
		}
	}
	names := make([]string, 0, len(matched))
	for n := range matched {
		names = append(names, n)
	}
	sort.Strings(names)
	sources := make([]any, 0, len(names))
	for _, n := range names {
		s := matched[n]
		sources = append(sources, obj("name", s.name, "label", s.label, "category", s.category, "region", s.region))
	}
	return obj("query", BuildQuery(icp), "source_count", len(sources), "sources", sources)
}

// columnMap is the column as the flat dict the people_search validator reads.
func columnMap(col wbcrud.Column) map[string]any {
	m := map[string]any{}
	for _, k := range col.Keys() {
		m[k] = col.Get(k)
	}
	return m
}

// PeopleSearchSource is the single discovery source a people_search preview lists.
func peopleSearchSource() *wbcrud.Obj {
	return obj("name", "crosslinked_ddg", "label", "LinkedIn profiles via DuckDuckGo (CrossLinked)",
		"category", "people", "region", nil)
}

// PreviewPeopleSearch is people_search.preview_people_search: how many companies and
// searches a run would use. No network.
func PreviewPeopleSearch(col map[string]any) *wbcrud.Obj {
	cfg, err := runplan.ValidatePeopleSearchConfig(col)
	if err != nil {
		if msg, ok := runplan.IsPeopleSearchError(err); ok {
			return obj("kind", PersonSource, "error", string(msg))
		}
		return obj("kind", PersonSource, "error", err.Error())
	}
	n := len(cfg.Companies)
	per := max(1, len(cfg.Titles))
	var companies, fromColumn, estimated any
	if n > 0 {
		companies = n
		estimated = min(int64(n*per), cfg.MaxSearches)
	}
	if cfg.FromColumn != "" {
		fromColumn = cfg.FromColumn
	}
	titles := make([]any, len(cfg.Titles))
	for i, t := range cfg.Titles {
		titles[i] = t
	}
	return obj("kind", PersonSource, "companies", companies, "from_column", fromColumn, "titles", titles,
		"max_per_company", cfg.MaxPerCompany, "max_searches", cfg.MaxSearches, "estimated_searches", estimated,
		"sources", []any{peopleSearchSource()}, "source_count", 1)
}

// Preview is the GET .../sources/{col}/preview body for a source column.
func Preview(col wbcrud.Column) *wbcrud.Obj {
	if k, _ := col.Get("kind").(string); k == PersonSource {
		return PreviewPeopleSearch(columnMap(col))
	}
	return PreviewSource(orNil(col.Get("icp")), orNil(col.Get("channels")))
}

// orNil is `x or {}` for the preview inputs: falsy values become nil.
func orNil(v any) any {
	if !truthy(v) {
		return nil
	}
	return v
}
