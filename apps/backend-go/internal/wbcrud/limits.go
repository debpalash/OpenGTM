package wbcrud

import "sync/atomic"

// Limits are request-shape caps beyond FastAPI (final-review hardening, env
// configurable through config.Flags): a request over a cap is a 422 and nothing
// is written.
type Limits struct {
	MaxAddRows     int // rows per add-rows request
	MaxRowKeys     int // keys in one added row's data
	MaxViewFilters int // filters per saved view
	MaxViewSorts   int // sorts per saved view
	MaxColumns     int // columns per workbook
}

// DefaultLimits are the caps used until SetLimits overrides them.
var DefaultLimits = Limits{MaxAddRows: 10000, MaxRowKeys: 2000, MaxViewFilters: 50, MaxViewSorts: 10, MaxColumns: 500}

var limits atomic.Pointer[Limits]

// CurrentLimits returns the active caps.
func CurrentLimits() Limits {
	if l := limits.Load(); l != nil {
		return *l
	}
	return DefaultLimits
}

// SetLimits overrides the caps; zero or negative fields keep their default.
func SetLimits(l Limits) {
	d := DefaultLimits
	if l.MaxAddRows > 0 {
		d.MaxAddRows = l.MaxAddRows
	}
	if l.MaxRowKeys > 0 {
		d.MaxRowKeys = l.MaxRowKeys
	}
	if l.MaxViewFilters > 0 {
		d.MaxViewFilters = l.MaxViewFilters
	}
	if l.MaxViewSorts > 0 {
		d.MaxViewSorts = l.MaxViewSorts
	}
	if l.MaxColumns > 0 {
		d.MaxColumns = l.MaxColumns
	}
	limits.Store(&d)
}
