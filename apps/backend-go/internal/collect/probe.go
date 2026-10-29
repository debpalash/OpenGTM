package collect

import (
	"context"
	"sync"
	"time"

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

// NativeProber is the leadjobs.Prober of GO_NATIVE_SOURCE_PROBE: the registry source list and
// the canary probes in process (no Python worker).
type NativeProber struct{ Web Web }

// SourceList is get_all_sources(enabled_only=False) names.
func (p NativeProber) SourceList(context.Context, time.Duration) ([]string, error) {
	return ProbeSourceNames(), nil
}

// SourceProbe probes one batch concurrently; a source whose probe did not finish within timeout
// answers nil (the health job then treats the run as an outage), as the RPC did.
func (p NativeProber) SourceProbe(ctx context.Context, names []string, timeout time.Duration) (map[string]*int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := make(map[string]*int, len(names))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range names {
		s, ok := SourceByName(n)
		if !ok {
			continue
		}
		goSafe(&wg, "probe "+n, func() {
			y := ProbeYield(ctx, p.Web, s)
			if ctx.Err() != nil {
				return
			}
			mu.Lock()
			out[n] = &y
			mu.Unlock()
		}, nil) // a crashed probe answers nil, as an unfinished one
	}
	wg.Wait()
	return out, nil
}
