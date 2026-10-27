package collect

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

// The native source-health probe (source_health._run_probe): replaces the Python worker's
// /internal/leadgen/source-list and /internal/leadgen/source-probe RPCs.

// CanaryQueries are CANARY_QUERIES: (query, city) pairs run through the live path per source.
var CanaryQueries = [][2]string{{"software companies", "Bangalore"}, {"marketing agency", "New York"}}

// ProbeMaxResults is PROBE_MAX_RESULTS (the live registry path's max_results).
const ProbeMaxResults = 8

// ProbeYield is _run_probe: for each canary, the first built query is searched and its non-junk
// hits counted; the yield is the best canary.
func ProbeYield(ctx context.Context, w Web, s Source) int {
	best := 0
	for _, c := range CanaryQueries {
		qs := BuildQueries(s, c[0], c[1])
		if len(qs) == 0 {
			continue
		}
		usable := 0
		for _, h := range w.Search(ctx, qs[0], ProbeMaxResults) {
			if !leadweb.IsJunkURL(h.Href) {
				usable++
			}
		}
		best = max(best, usable)
	}
	return best
}

// ProbeSourceNames is get_all_sources(enabled_only=False): every registry source, disabled ones
// included so they can recover.
func ProbeSourceNames() []string {
	var out []string
	for _, s := range AllSources("", false) {
		out = append(out, s.Name)
	}
	return out
}

// SourceByName is source_registry.get_source.
func SourceByName(name string) (Source, bool) {
	for _, s := range registry {
		if s.Name == name {
			return s, true
		}
	}
	return Source{}, false
}
