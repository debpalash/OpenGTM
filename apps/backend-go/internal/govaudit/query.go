package govaudit

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

const (
	// MaxLimit caps a page (Python: Query(le=500)).
	MaxLimit = 500
	// DefaultLimit is Python's default page size.
	DefaultLimit = 100
)

// Filter narrows audit events. Method is upper-cased before comparing; empty
// Method/Outcome mean "no filter" (Python truthiness).
type Filter struct {
	ActorUserID   *int32
	Method        string
	Outcome       string
	Since, Before *time.Time
}

// Event is one audit row as the API renders it.
type Event struct {
	ID             string
	ActorUserID    *int32
	ActorRole      string
	Method         string
	Route          string
	ResourcePath   string
	ResponseStatus int32
	Outcome        string
	RequestID      string
	Metadata       json.RawMessage
	CreatedAt      time.Time
}

// MarshalJSON emits Python's key order with a naive isoformat created_at.
func (e Event) MarshalJSON() ([]byte, error) {
	md := e.Metadata
	if len(md) == 0 {
		md = json.RawMessage("{}")
	}
	return json.Marshal(struct {
		ID             string          `json:"id"`
		ActorUserID    *int32          `json:"actor_user_id"`
		ActorRole      string          `json:"actor_role"`
		Method         string          `json:"method"`
		Route          string          `json:"route"`
		ResourcePath   string          `json:"resource_path"`
		ResponseStatus int32           `json:"response_status"`
		Outcome        string          `json:"outcome"`
		RequestID      string          `json:"request_id"`
		Metadata       json.RawMessage `json:"metadata"`
		CreatedAt      string          `json:"created_at"`
	}{e.ID, e.ActorUserID, e.ActorRole, e.Method, e.Route, e.ResourcePath, e.ResponseStatus, e.Outcome, e.RequestID, md, isoformat(e.CreatedAt)})
}

// Page is one keyset page.
type Page struct {
	Events     []Event
	NextCursor *string
	NextBefore *time.Time
	HasMore    bool
	Limit      int
}

// MarshalJSON emits events (never null), next_cursor, next_before, has_more, limit.
func (p Page) MarshalJSON() ([]byte, error) {
	ev := p.Events
	if ev == nil {
		ev = []Event{}
	}
	var nb *string
	if p.NextBefore != nil {
		s := isoformat(*p.NextBefore)
		nb = &s
	}
	return json.Marshal(struct {
		Events     []Event `json:"events"`
		NextCursor *string `json:"next_cursor"`
		NextBefore *string `json:"next_before"`
		HasMore    bool    `json:"has_more"`
		Limit      int     `json:"limit"`
	}{ev, p.NextCursor, nb, p.HasMore, p.Limit})
}

// splitTime routes a filter time to the column-compatible parameter: times
// located in time.UTC are naive (plain timestamp compare), anything else is
// aware (timestamptz compare in the session TimeZone, like Python).
func splitTime(t *time.Time) (naive pgtype.Timestamp, aware pgtype.Timestamptz) {
	if t == nil {
		return
	}
	if t.Location() == time.UTC {
		return pgtype.Timestamp{Time: *t, Valid: true}, pgtype.Timestamptz{}
	}
	return pgtype.Timestamp{}, pgtype.Timestamptz{Time: *t, Valid: true}
}

func params(ws string, f Filter, limit int) gen.ListGovernanceAuditEventsParams {
	p := gen.ListGovernanceAuditEventsParams{
		WorkspaceID: ws,
		Method:      strings.ToUpper(f.Method),
		Outcome:     f.Outcome,
		RowLimit:    int32(limit),
	}
	p.Since, p.SinceTz = splitTime(f.Since)
	p.Before, p.BeforeTz = splitTime(f.Before)
	if f.ActorUserID != nil {
		p.ActorUserID = pgtype.Int4{Int32: *f.ActorUserID, Valid: true}
	}
	return p
}

func toEvent(r gen.ListGovernanceAuditEventsRow) (Event, error) {
	md, err := normalizeMetadata(r.MetadataJson)
	if err != nil {
		return Event{}, err
	}
	e := Event{ID: r.ID, ActorRole: r.ActorRole, Method: r.Method, Route: r.Route, ResourcePath: r.ResourcePath,
		ResponseStatus: r.ResponseStatus, Outcome: r.Outcome, RequestID: r.RequestID, Metadata: md, CreatedAt: r.CreatedAt.Time}
	if r.ActorUserID.Valid {
		v := r.ActorUserID.Int32
		e.ActorUserID = &v
	}
	return e, nil
}

// List returns one page of ws's audit events, newest first (created_at DESC,
// id DESC). Aware since/before and every cursor compare as timestamptz in the
// session TimeZone (Python parity); naive since/before as plain timestamps. When cursor is non-empty it takes precedence over f.Before. limit
// is clamped to 1..MaxLimit. tx must be inside tenancy.InWorkspace for ws; ws
// is also an explicit predicate. A bad cursor is a *CursorError (422).
func List(ctx context.Context, tx pgx.Tx, ws string, f Filter, cursor string, limit int) (Page, error) {
	if limit < 1 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	p := params(ws, f, limit+1)
	if cursor != "" {
		ct, cid, cerr := DecodeCursor(cursor, "audit event")
		if cerr != nil {
			return Page{}, cerr
		}
		p.Before, p.BeforeTz = pgtype.Timestamp{}, pgtype.Timestamptz{}
		// Python decodes every cursor to an aware datetime (naive -> UTC), so it
		// always compares as timestamptz.
		p.CursorTz = pgtype.Timestamptz{Time: ct, Valid: true}
		p.CursorID = pgtype.Text{String: cid, Valid: true}
	}
	rows, err := gen.New(tx).ListGovernanceAuditEvents(ctx, p)
	if err != nil {
		return Page{}, err
	}
	page := Page{Events: make([]Event, 0, len(rows)), Limit: limit}
	if len(rows) > limit {
		page.HasMore = true
		rows = rows[:limit]
	}
	for _, r := range rows {
		e, err := toEvent(r)
		if err != nil {
			return Page{}, err
		}
		page.Events = append(page.Events, e)
	}
	if page.HasMore {
		last := page.Events[len(page.Events)-1]
		c := EncodeCursor(last.CreatedAt, last.ID)
		page.NextCursor = &c
		t := last.CreatedAt
		page.NextBefore = &t
	}
	return page, nil
}
