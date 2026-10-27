package collect

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
)

// Source is one source_registry.SOURCES entry (sources.json is dumped from Python by
// testdata/record.py).
type Source struct {
	Name               string   `json:"name"`
	Label              string   `json:"label"`
	Region             []string `json:"region"`
	Category           string   `json:"category"`
	SiteDomain         *string  `json:"site_domain"`
	QueryTemplates     []string `json:"query_templates"`
	ExtractFromListing bool     `json:"extract_from_listing"`
	Priority           int      `json:"priority"`
	Enabled            bool     `json:"enabled"`
}

// Site is site_domain ("" for None).
func (s Source) Site() string {
	if s.SiteDomain == nil {
		return ""
	}
	return *s.SiteDomain
}

//go:embed sources.json
var sourcesJSON []byte

var registry = func() []Source {
	var out []Source
	if err := json.Unmarshal(sourcesJSON, &out); err != nil {
		panic("collect: sources.json: " + err.Error())
	}
	return out
}()

// AllSources is get_all_sources(region, category="", enabled_only): filtered and stably sorted by
// priority. region "" means no region filter.
func AllSources(region string, enabledOnly bool) []Source {
	var out []Source
	for _, s := range registry {
		if enabledOnly && !s.Enabled {
			continue
		}
		if region != "" && !contains(s.Region, region) {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// BuildQueries is build_queries: each template with {q}, {city} and {industry} filled in
// (industry = the lower-cased query without the city and with " in " collapsed), stripped, empty
// results dropped.
func BuildQueries(s Source, query, city string) []string {
	industry := pyLower(query)
	if city != "" {
		industry = pyStrip(strings.ReplaceAll(strings.ReplaceAll(industry, pyLower(city), ""), " in ", " "))
	}
	r := strings.NewReplacer("{q}", query, "{city}", city, "{industry}", industry)
	var out []string
	for _, t := range s.QueryTemplates {
		if q := pyStrip(r.Replace(t)); q != "" {
			out = append(out, q)
		}
	}
	return out
}
