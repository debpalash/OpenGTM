package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Audience refresh, members and events (routers/audiences.py). Order as FastAPI: authentication, workspace, role (403,
// refresh only), query validation (422), then the audience lookup (404).

// audReadRefreshTimeout bounds POST /{id}/refresh (plan 10 D8); tests shorten it.
var audReadRefreshTimeout = audiencerefresh.RouteTimeout

// registerAudiencesRead registers POST /{id}/refresh and GET /{id}/members, /{id}/events.
func registerAudiencesRead(r *httpx.Router, d Deps) {
	h := audRead{audCrud: audCrud{d: d}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("audiences.write", "editor", "admin")
	r.Handle("POST /api/audiences/{audience_id}/refresh", http.HandlerFunc(h.refresh), authn, workspace, editor)
	r.Handle("GET /api/audiences/{audience_id}/members", http.HandlerFunc(h.members), authn, workspace)
	r.Handle("GET /api/audiences/{audience_id}/events", http.HandlerFunc(h.events), authn, workspace)
}

// audRead reuses the CRUD handler's cohort rules and error mapping.
type audRead struct{ audCrud }

func (h audRead) refresh(w http.ResponseWriter, r *http.Request) {
	id, ok := h.audienceID(w, r)
	if !ok {
		return
	}
	ws := h.ws(r).WorkspaceID
	// Concurrent refreshes of one audience serialize on its row lock inside the engine (single-flight).
	res, err := audiencerefresh.RefreshObserved(r.Context(), h.d.Pool, ws, id, audiencerefresh.Deps{
		AudienceEvents: h.d.AudienceEvents,
		ExecutorFor:    goExecutor,
		PageSize:       h.d.Flags.Audiences.RefreshPageSize,
		Timeout:        audReadRefreshTimeout,
	})
	if err != nil {
		h.refreshFailed(w, err)
		return
	}
	res.Audience.Filters = pyJSONText(string(res.Audience.Filters), `{}`) // decode + re-dump like the CRUD routes
	httpx.WriteJSON(w, http.StatusOK, res)
}

// audParseInt is pydantic v2 lax int parsing of a query string: whitespace, sign, underscores between digits, ".0".
func audParseInt(s string) (*big.Int, bool) {
	s = strings.Trim(s, " \t\n\r\v\f")
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	ip, fp, hasFrac := strings.Cut(s, ".")
	if hasFrac && (strings.Trim(fp, "0") != "" || fp == "") {
		return nil, false
	}
	if ip == "" || ip[0] == '_' || ip[len(ip)-1] == '_' || strings.Contains(ip, "__") {
		return nil, false
	}
	ip = strings.ReplaceAll(ip, "_", "")
	for _, c := range ip {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	v, ok := new(big.Int).SetString(ip, 10)
	if !ok {
		return nil, false
	}
	if neg {
		v.Neg(v)
	}
	return v, true
}

// audIntParam validates one optional bounded int query parameter (last value wins). max < 0 means unbounded.
// present is false when absent; the value is clamped to int64 for huge (valid) inputs.
func audIntParam(q url.Values, name string, def, min, max int64, errs *[]httpx.FieldError) (val int64, present bool) {
	vs, ok := q[name]
	if !ok || len(vs) == 0 {
		return def, false
	}
	raw := vs[len(vs)-1]
	loc := []any{"query", name}
	v, ok := audParseInt(raw)
	if !ok {
		*errs = append(*errs, httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
			Msg: "Input should be a valid integer, unable to parse string as an integer"})
		return def, false
	}
	if v.Cmp(big.NewInt(min)) < 0 {
		*errs = append(*errs, httpx.FieldError{Type: "greater_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be greater than or equal to " + big.NewInt(min).String(), Ctx: map[string]any{"ge": min}})
		return def, false
	}
	if max >= 0 && v.Cmp(big.NewInt(max)) > 0 {
		*errs = append(*errs, httpx.FieldError{Type: "less_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be less than or equal to " + big.NewInt(max).String(), Ctx: map[string]any{"le": max}})
		return def, false
	}
	if !v.IsInt64() {
		return math.MaxInt64, true
	}
	return v.Int64(), true
}

type audPage struct {
	limit, offset int32
	before        pgtype.Int8
}

// parsePage validates limit (1..maxLimit, default defLimit), before_id (>= 1) and, for members, offset (0..100000)
// in declaration order, collecting every error.
func parseAudPage(q url.Values, defLimit, maxLimit int64, withOffset bool) (audPage, []httpx.FieldError) {
	var errs []httpx.FieldError
	var p audPage
	l, _ := audIntParam(q, "limit", defLimit, 1, maxLimit, &errs)
	b, hasB := audIntParam(q, "before_id", 0, 1, -1, &errs)
	var off int64
	if withOffset {
		off, _ = audIntParam(q, "offset", 0, 0, 100000, &errs)
	}
	p.limit, p.offset = int32(l), int32(off)
	if hasB {
		p.before = pgtype.Int8{Int64: b, Valid: true}
	}
	return p, errs
}

// audReadISO is a naive datetime as FastAPI renders it: microseconds only when non-zero.
func audReadISO(t pgtype.Timestamp) string {
	if p := audISO(t); p != nil {
		return *p
	}
	return ""
}

// audSnapshot is `snapshot or {}` after Python's decode and FastAPI re-dump (pyJSONText: key order kept, no HTML or
// non-ASCII escaping, float repr; every falsy value becomes {}).
func audSnapshot(raw []byte) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`{}`)
	}
	return pyJSONText(string(raw), `{}`)
}

type audMemberOut struct {
	ID         int32           `json:"id"`
	LeadID     int32           `json:"lead_id"`
	Snapshot   json.RawMessage `json:"snapshot"`
	JoinedAt   string          `json:"joined_at"`
	LastSeenAt string          `json:"last_seen_at"`
}

type audEventOut struct {
	ID         int32           `json:"id"`
	AudienceID string          `json:"audience_id"`
	LeadID     int32           `json:"lead_id"`
	EventType  string          `json:"event_type"`
	Snapshot   json.RawMessage `json:"snapshot"`
	CreatedAt  string          `json:"created_at"`
}

// list runs the existence check and the page query in one workspace transaction.
func (h audRead) list(w http.ResponseWriter, r *http.Request, withOffset bool, defLimit, maxLimit int64,
	fetch func(q *gen.Queries, r *http.Request, ws, id string, p audPage) (any, error)) {
	p, errs := parseAudPage(r.URL.Query(), defLimit, maxLimit, withOffset)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	id, ok := h.audienceID(w, r)
	if !ok {
		return
	}
	ws := h.ws(r).WorkspaceID
	var out any
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		found, err := q.AudReadExists(r.Context(), gen.AudReadExistsParams{ID: id, WorkspaceID: ws})
		if err != nil {
			return err
		}
		if !found {
			return pgx.ErrNoRows
		}
		out, err = fetch(q, r, ws, id, p)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Detail(w, http.StatusNotFound, audNotFound)
		return
	}
	if err != nil {
		h.internal(w, "read", err)
		return
	}
	writePy(w, out) // raw U+2028/2029 like Python's json.dumps(ensure_ascii=False)
}

func (h audRead) members(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, true, 200, 1000, func(q *gen.Queries, r *http.Request, ws, id string, p audPage) (any, error) {
		rows, err := q.AudReadMembers(r.Context(), gen.AudReadMembersParams{WorkspaceID: ws, AudienceID: id, BeforeID: p.before,
			RowOffset: p.offset, RowLimit: p.limit})
		out := make([]audMemberOut, 0, len(rows))
		for _, m := range rows {
			out = append(out, audMemberOut{ID: m.ID, LeadID: m.LeadID, Snapshot: audSnapshot(m.Snapshot),
				JoinedAt: audReadISO(m.JoinedAt), LastSeenAt: audReadISO(m.LastSeenAt)})
		}
		return out, err
	})
}

func (h audRead) events(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, false, 100, 500, func(q *gen.Queries, r *http.Request, ws, id string, p audPage) (any, error) {
		rows, err := q.AudReadEvents(r.Context(), gen.AudReadEventsParams{WorkspaceID: ws, AudienceID: id, BeforeID: p.before, RowLimit: p.limit})
		out := make([]audEventOut, 0, len(rows))
		for _, e := range rows {
			out = append(out, audEventOut{ID: e.ID, AudienceID: e.AudienceID, LeadID: e.LeadID, EventType: e.EventType,
				Snapshot: audSnapshot(e.Snapshot), CreatedAt: audReadISO(e.CreatedAt)})
		}
		return out, err
	})
}
