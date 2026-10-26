package audiencerefresh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencevalid"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
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

// PyTyper names the Python exception class a failure maps to, for the
// "<Type>: <msg>" health text.
type PyTyper interface{ PyType() string }

// dataError is a refresh failure Python raises as a ValueError/DataError.
type dataError struct{ typ, msg string }

func (e *dataError) Error() string  { return e.msg }
func (e *dataError) PyType() string { return e.typ }

func (d Deps) withDefaults() Deps {
	if d.PageSize <= 0 {
		d.PageSize = PageSize
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.NewID == nil {
		d.NewID = newUUID4
	}
	if d.Leads == nil {
		d.Leads = defaultLeads
	}
	return d
}

func newUUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// defaultLeads is leadsread.QueryLeads over the audience filter; snapshots are the
// lead rows' JSON as query_leads_page returns them.
func defaultLeads(ctx context.Context, tx pgx.Tx, filters map[string]any, page, pageSize int) ([]json.RawMessage, int, error) {
	f, err := audiencevalid.ToLeadFilter(filters)
	if err != nil {
		return nil, 0, err
	}
	leads, total, err := leadsread.QueryLeads(ctx, tx, f, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	out := make([]json.RawMessage, len(leads))
	for i, l := range leads {
		if out[i], err = json.Marshal(l); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

func decodeNumbers(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func decodeFilters(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	v, err := decodeNumbers(raw)
	if err != nil {
		return nil, fmt.Errorf("audiencerefresh: decode filters: %w", err)
	}
	switch m := v.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return m, nil
	}
	return nil, &dataError{"TypeError", "audience filters must be an object"}
}

// rowID is int(row["id"]); ok is false when the row has no id (None or absent).
func rowID(row map[string]any) (id int64, ok bool, err error) {
	v, present := row["id"]
	if !present || v == nil {
		return 0, false, nil
	}
	bad := func() (int64, bool, error) {
		return 0, false, &dataError{"ValueError", fmt.Sprintf("invalid literal for int() with base 10: %v", v)}
	}
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true, nil
		}
		return 0, true, nil
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return n, true, nil
		}
		f, err := x.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return bad()
		}
		return int64(f), true, nil
	case string:
		n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(x), "_", ""), 10, 64)
		if err != nil {
			return bad()
		}
		return n, true, nil
	}
	return bad()
}

// pyEqual is Python's == over decoded JSON (UseNumber): numbers by value (1 == 1.0,
// True == 1), dicts irrespective of key order.
func pyEqual(a, b any) bool {
	if x, ok := numVal(a); ok {
		y, ok := numVal(b)
		return ok && x.Cmp(y) == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !pyEqual(xv, yv) {
				return false
			}
		}
		return true
	}
	return false
}

// numVal returns an exact rational for numbers and bools; non-finite floats
// (json.loads accepts 1e400 as inf) map to a huge sentinel via big.Float-free path.
func numVal(v any) (*big.Rat, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewRat(1, 1), true
		}
		return new(big.Rat), true
	case json.Number:
		s := string(x)
		if n, ok := new(big.Int).SetString(s, 10); ok {
			return new(big.Rat).SetInt(n), true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, false
		}
		if math.IsInf(f, 0) {
			r := new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 5000))
			if f < 0 {
				r.Neg(r)
			}
			return r, true
		}
		return new(big.Rat).SetFloat64(f), true
	}
	return nil, false
}

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

func i32(id int64) (int32, error) {
	if id < math.MinInt32 || id > math.MaxInt32 {
		return 0, &dataError{"DataError", fmt.Sprintf("lead id %d out of range for integer", id)}
	}
	return int32(id), nil
}

func snapshotOrEmpty(b []byte) []byte {
	if t := bytes.TrimSpace(b); len(t) == 0 || string(t) == "null" {
		return []byte("{}")
	}
	return b
}

type counts struct{ entered, changed, exited, matched int }

// Refresh is refresh_audience: tx 1 locks the audience row and applies the page-bounded
// diff atomically; after commit the events are emitted in keyset pages and the
// destination syncs enqueued (each in its own transaction, so a failure there never
// undoes the committed diff).
func Refresh(ctx context.Context, pool *pgxpool.Pool, wsID, audienceID string, deps Deps) (Result, error) {
	deps = deps.withDefaults()
	if deps.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deps.Timeout)
		defer cancel()
	}
	return refresh(ctx, pool, wsID, audienceID, deps)
}

func refresh(ctx context.Context, pool *pgxpool.Pool, wsID, audienceID string, deps Deps) (Result, error) {
	if wsID == "" {
		return Result{}, tenancy.ErrNoWorkspace
	}
	refreshID := deps.NewID()
	now := deps.Now().UTC()
	var c counts
	err := tenancy.InWorkspace(ctx, pool, wsID, func(tx pgx.Tx) error {
		var err error
		c, err = applyDiff(ctx, tx, wsID, audienceID, refreshID, now, deps)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	if c.entered > 0 || c.exited > 0 {
		if err := emitEvents(ctx, pool, wsID, audienceID, refreshID, deps); err != nil {
			return Result{}, err
		}
	}
	if c.entered > 0 || c.exited > 0 || c.changed > 0 {
		err := tenancy.InWorkspace(ctx, pool, wsID, func(tx pgx.Tx) error {
			_, err := deststore.EnqueueAudienceSyncs(ctx, tx, wsID, audienceID, deps.ExecutorFor)
			return err
		})
		if err != nil {
			return Result{}, err
		}
	}
	a, err := readAudience(ctx, pool, wsID, audienceID)
	if err != nil {
		return Result{}, err
	}
	return Result{Audience: a, Entered: c.entered, Exited: c.exited, Unchanged: c.matched - c.entered - c.changed, Changed: c.changed}, nil
}

func applyDiff(ctx context.Context, tx pgx.Tx, ws, audienceID, refreshID string, now time.Time, deps Deps) (counts, error) {
	var c counts
	q := gen.New(tx)
	row, err := q.AudRefreshLock(ctx, gen.AudRefreshLockParams{ID: audienceID, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("audiencerefresh: lock audience: %w", err)
	}
	filters, err := decodeFilters(row.Filters)
	if err != nil {
		return c, err
	}
	for page := 1; ; page++ {
		rows, total, err := deps.Leads(ctx, tx, filters, page, deps.PageSize)
		if err != nil {
			return c, err
		}
		var order []int64
		snaps := map[int64]json.RawMessage{}
		for _, r := range rows {
			v, err := decodeNumbers(r)
			if err != nil {
				return c, fmt.Errorf("audiencerefresh: decode lead row: %w", err)
			}
			m, _ := v.(map[string]any)
			id, ok, err := rowID(m)
			if err != nil {
				return c, err
			}
			if !ok {
				continue
			}
			if _, seen := snaps[id]; !seen {
				order = append(order, id)
			}
			snaps[id] = r // a duplicate id keeps its first position, last row wins
		}
		existing := map[int64]gen.AudRefreshMembersByLeadsRow{}
		if len(order) > 0 {
			ids := make([]int32, len(order))
			for i, id := range order {
				if ids[i], err = i32(id); err != nil {
					return c, err
				}
			}
			ms, err := q.AudRefreshMembersByLeads(ctx, gen.AudRefreshMembersByLeadsParams{WorkspaceID: ws, AudienceID: audienceID, LeadIds: ids})
			if err != nil {
				return c, fmt.Errorf("audiencerefresh: load members: %w", err)
			}
			for _, m := range ms {
				existing[int64(m.LeadID)] = m
			}
		}
		for _, id := range order {
			snap := snaps[id]
			lead, _ := i32(id)
			if m, ok := existing[id]; !ok {
				if err := q.AudRefreshMemberInsert(ctx, gen.AudRefreshMemberInsertParams{WorkspaceID: ws, AudienceID: audienceID,
					LeadID: lead, Snapshot: snap, RefreshToken: refreshID, Now: tstz(now)}); err != nil {
					return c, fmt.Errorf("audiencerefresh: insert member: %w", err)
				}
				if err := q.AudRefreshEventInsert(ctx, gen.AudRefreshEventInsertParams{WorkspaceID: ws, AudienceID: audienceID,
					LeadID: lead, EventType: "entered", Snapshot: snap, RefreshID: refreshID}); err != nil {
					return c, fmt.Errorf("audiencerefresh: insert event: %w", err)
				}
				c.entered++
			} else {
				old, err := decodeNumbers(snapshotOrEmpty(m.Snapshot))
				if err != nil {
					return c, fmt.Errorf("audiencerefresh: decode member snapshot: %w", err)
				}
				cur, err := decodeNumbers(snap)
				if err != nil {
					return c, err
				}
				if !pyEqual(old, cur) {
					c.changed++
				}
				if err := q.AudRefreshMemberTouch(ctx, gen.AudRefreshMemberTouchParams{ID: m.ID, WorkspaceID: ws, AudienceID: audienceID,
					Snapshot: snap, RefreshToken: refreshID, Now: tstz(now)}); err != nil {
					return c, fmt.Errorf("audiencerefresh: touch member: %w", err)
				}
			}
		}
		c.matched += len(order)
		if c.matched >= total || len(rows) == 0 {
			break
		}
	}

	var after int32
	for {
		ms, err := q.AudRefreshUnseenMembers(ctx, gen.AudRefreshUnseenMembersParams{WorkspaceID: ws, AudienceID: audienceID,
			RefreshToken: refreshID, AfterID: after, PageLimit: int32(deps.PageSize)})
		if err != nil {
			return c, fmt.Errorf("audiencerefresh: list exits: %w", err)
		}
		if len(ms) == 0 {
			break
		}
		ids := make([]int32, len(ms))
		for i, m := range ms {
			ids[i] = m.ID
			if err := q.AudRefreshEventInsert(ctx, gen.AudRefreshEventInsertParams{WorkspaceID: ws, AudienceID: audienceID,
				LeadID: m.LeadID, EventType: "exited", Snapshot: snapshotOrEmpty(m.Snapshot), RefreshID: refreshID}); err != nil {
				return c, fmt.Errorf("audiencerefresh: insert exit event: %w", err)
			}
			c.exited++
		}
		if err := q.AudRefreshMembersDelete(ctx, gen.AudRefreshMembersDeleteParams{WorkspaceID: ws, AudienceID: audienceID, Ids: ids}); err != nil {
			return c, fmt.Errorf("audiencerefresh: delete members: %w", err)
		}
		after = ms[len(ms)-1].ID
	}
	if err := q.AudRefreshFinish(ctx, gen.AudRefreshFinishParams{ID: audienceID, WorkspaceID: ws,
		MemberCount: int32(c.matched), RefreshedAt: tstz(now)}); err != nil {
		return c, fmt.Errorf("audiencerefresh: finish: %w", err)
	}
	return c, nil
}
