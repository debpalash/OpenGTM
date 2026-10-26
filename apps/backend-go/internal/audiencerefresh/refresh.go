package audiencerefresh

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PageSize is AUDIENCE_REFRESH_PAGE_SIZE.
const PageSize = 500

// Timeouts bounding a whole refresh: the manual route and the queue job.
const (
	RouteTimeout = 120 * time.Second
	JobTimeout   = 900 * time.Second
)

// ErrNotFound means the audience does not exist in the workspace.
var ErrNotFound = errors.New("audiencerefresh: audience not found")

// LeadSource is query_leads_page: one page of snapshots (each a JSON object, key
// order kept) plus the unpaged total. filters is the audience filter object decoded
// with UseNumber ({} for NULL).
type LeadSource func(ctx context.Context, tx pgx.Tx, filters map[string]any, page, pageSize int) (rows []json.RawMessage, total int, err error)

// Deps are the refresh collaborators; the zero value is production behaviour
// except AudienceEvents (nil emits nothing) and ExecutorFor (nil: python).
type Deps struct {
	AudienceEvents Emitter
	// ExecutorFor picks the sync job executor per destination type (the cohort rule).
	ExecutorFor func(destType string) string
	// PageSize bounds lead, exit and event paging; 0 means PageSize (500).
	PageSize int
	// Leads defaults to leadsread.QueryLeads over audiencevalid.ToLeadFilter.
	Leads LeadSource
	Now   func() time.Time
	NewID func() string
	// Timeout bounds the whole call when > 0 (RouteTimeout / JobTimeout).
	Timeout time.Duration
}

// Result is refresh_audience's return value.
type Result struct {
	Audience  Audience `json:"audience"`
	Entered   int      `json:"entered"`
	Exited    int      `json:"exited"`
	Unchanged int      `json:"unchanged"`
	Changed   int      `json:"changed"`
}

// Refresh is refresh_audience.
func Refresh(ctx context.Context, pool *pgxpool.Pool, wsID, audienceID string, deps Deps) (Result, error) {
	return Result{}, errors.New("audiencerefresh: not implemented")
}

// RefreshObserved is refresh_audience_observed.
func RefreshObserved(ctx context.Context, pool *pgxpool.Pool, wsID, audienceID string, deps Deps) (Result, error) {
	return Result{}, errors.New("audiencerefresh: not implemented")
}

// PyTyper names the Python exception class a failure maps to, for the
// "<Type>: <msg>" health text.
type PyTyper interface{ PyType() string }
