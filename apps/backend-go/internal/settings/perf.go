package settings

import (
	"context"
	"strconv"
	"strings"
)

// PerfDefaults mirrors routers/settings.py _ENRICHMENT_DEFAULTS.
type PerfDefaults struct {
	RowConcurrency  int `json:"row_concurrency"`
	ProviderWorkers int `json:"provider_workers"`
	ProviderTimeout int `json:"provider_timeout"`
	MaxProviders    int `json:"max_providers"`
	RetryPasses     int `json:"retry_passes"`
}

// PerfSettings is the body of GET/PUT /api/settings/enrichment-perf.
type PerfSettings struct {
	RowConcurrency  int          `json:"row_concurrency"`
	ProviderWorkers int          `json:"provider_workers"`
	ProviderTimeout int          `json:"provider_timeout"`
	MaxProviders    int          `json:"max_providers"`
	RetryPasses     int          `json:"retry_passes"`
	Defaults        PerfDefaults `json:"defaults"`
}

// PerfUpdate is the PUT body; nil means "not provided" (null is ignored).
type PerfUpdate struct {
	RowConcurrency, ProviderWorkers, ProviderTimeout, MaxProviders, RetryPasses *int64
}

// Perf serves the enrichment performance settings (ENRICH_* keys of app_settings).
type Perf struct {
	App *App
	Reg *Registry
}

type perfKey struct {
	name     string
	lo, hi   int64
	def      func(PerfDefaults) int
	field    func(*PerfSettings) *int
	provided func(PerfUpdate) *int64
}

var perfKeys = []perfKey{
	{"row_concurrency", 1, 64, func(d PerfDefaults) int { return d.RowConcurrency }, func(s *PerfSettings) *int { return &s.RowConcurrency }, func(u PerfUpdate) *int64 { return u.RowConcurrency }},
	{"provider_workers", 1, 64, func(d PerfDefaults) int { return d.ProviderWorkers }, func(s *PerfSettings) *int { return &s.ProviderWorkers }, func(u PerfUpdate) *int64 { return u.ProviderWorkers }},
	{"provider_timeout", 3, 120, func(d PerfDefaults) int { return d.ProviderTimeout }, func(s *PerfSettings) *int { return &s.ProviderTimeout }, func(u PerfUpdate) *int64 { return u.ProviderTimeout }},
	{"max_providers", 0, 20, func(d PerfDefaults) int { return d.MaxProviders }, func(s *PerfSettings) *int { return &s.MaxProviders }, func(u PerfUpdate) *int64 { return u.MaxProviders }},
	{"retry_passes", 0, 5, func(d PerfDefaults) int { return d.RetryPasses }, func(s *PerfSettings) *int { return &s.RetryPasses }, func(u PerfUpdate) *int64 { return u.RetryPasses }},
}

func (p *Perf) defaults() PerfDefaults {
	d := func(k string) int { v, _ := p.Reg.EnrichmentDefaults.Get(k); return v }
	return PerfDefaults{
		RowConcurrency: d("row_concurrency"), ProviderWorkers: d("provider_workers"),
		ProviderTimeout: d("provider_timeout"), MaxProviders: d("max_providers"), RetryPasses: d("retry_passes"),
	}
}

// Get reads each ENRICH_<KEY> through App.Get; an unset, empty or non-integer
// value yields the default (_int_setting). Stored values are not re-clamped.
func (p *Perf) Get(ctx context.Context) (PerfSettings, error) {
	d := p.defaults()
	out := PerfSettings{Defaults: d}
	for _, k := range perfKeys {
		v, err := p.App.Get(ctx, "ENRICH_"+strings.ToUpper(k.name), "")
		if err != nil {
			return PerfSettings{}, err
		}
		n := k.def(d)
		if v != "" {
			if parsed, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64); perr == nil {
				n = int(parsed)
			}
		}
		*k.field(&out) = n
	}
	return out, nil
}

// Put clamps every provided value to its bounds, stores it as a decimal string
// and returns the resulting settings.
func (p *Perf) Put(ctx context.Context, u PerfUpdate) (PerfSettings, error) {
	for _, k := range perfKeys {
		v := k.provided(u)
		if v == nil {
			continue
		}
		n := min(k.hi, max(k.lo, *v))
		if err := p.App.Set(ctx, "ENRICH_"+strings.ToUpper(k.name), strconv.FormatInt(n, 10)); err != nil {
			return PerfSettings{}, err
		}
	}
	return p.Get(ctx)
}
