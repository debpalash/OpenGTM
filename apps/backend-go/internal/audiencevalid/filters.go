// Package audiencevalid ports the audience filter validation, account-key and
// accounts-rollup helpers of apps/api/routers/audiences.py.
package audiencevalid

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"

// FilterKeys are the audience filter keys Python accepts.
var FilterKeys []string

// ValueError is Python's ValueError raised by _validate_filters.
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

func ValidateFilters(v map[string]any) (map[string]any, error) { return nil, nil }

func ToLeadFilter(f map[string]any) (leadsread.Filter, error) { return leadsread.Filter{}, nil }
