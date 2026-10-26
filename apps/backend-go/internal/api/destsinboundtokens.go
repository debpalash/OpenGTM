package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destinbound"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// POST/GET/DELETE /api/audience-destinations/{id}/inbound-token and GET .../inbound-receipts (routers/destinations.py).
// Like FastAPI the order is authentication, workspace, role (403, admin / secrets.manage for the token routes), query
// validation (422), then the destination lookup (404) and the cursor decode (422).
//
// Security: the raw token exists only in the rotate response body. It is never persisted (only its SHA-256), logged,
// put in an error or in the audit metadata (the audit middleware records method, path and status only).

const (
	destTokNotFound = "Destination not found"
	destTokNotCRM   = "Inbound sync tokens are only available for CRM destinations"
	destTokWarning  = "Copy this token now; it will not be shown again."
)

var errDestTokNotCRM = errors.New("destination is not a CRM")

// registerDestsInboundTokens registers the four token and receipt routes.
func registerDestsInboundTokens(r *httpx.Router, d Deps) {
	h := destTokens{d: d}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	admin := identity.RequireAdmin()
	const tok = "/api/audience-destinations/{destination_id}/inbound-token"
	r.Handle("POST "+tok, http.HandlerFunc(h.rotate), authn, workspace, admin)
	r.Handle("GET "+tok, http.HandlerFunc(h.status), authn, workspace, admin)
	r.Handle("DELETE "+tok, http.HandlerFunc(h.revoke), authn, workspace, admin)
	r.Handle("GET /api/audience-destinations/{destination_id}/inbound-receipts", http.HandlerFunc(h.receipts), authn, workspace)
}

type destTokens struct{ d Deps }

func (h destTokens) internal(w http.ResponseWriter, what string, err error) {
	h.d.logger().Error("destinations: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// destID is the path id; one Postgres cannot store (NUL, invalid UTF-8) matches nothing.
func (h destTokens) destID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("destination_id")
	if strings.ContainsRune(id, 0) || !utf8.ValidString(id) {
		httpx.Detail(w, http.StatusNotFound, destTokNotFound)
		return "", false
	}
	return id, true
}

func destTokIsCRM(t string) bool { return t == "hubspot" || t == "salesforce" }

// fail maps the store errors of the token routes.
func (h destTokens) fail(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Detail(w, http.StatusNotFound, destTokNotFound)
	case errors.Is(err, errDestTokNotCRM):
		httpx.Detail(w, http.StatusConflict, destTokNotCRM)
	default:
		h.internal(w, what, err)
	}
}

// destTokTZ renders an aware instant like datetime.isoformat() (microseconds only when non-zero, then +00:00).
func destTokTZ(t time.Time) string { return naiveISO(t.UTC()) + "+00:00" }

func destTokTZPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := destTokTZ(*t)
	return &s
}

type destTokRotated struct {
	Token     string `json:"token"`
	Prefix    string `json:"prefix"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Warning   string `json:"warning"`
}

func (h destTokens) rotate(w http.ResponseWriter, r *http.Request) {
	id, ok := h.destID(w, r)
	if !ok {
		return
	}
	c, _ := identity.WorkspaceFrom(r.Context())
	ws := c.WorkspaceID
	var res destinbound.RotateResult
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		// The row lock serializes concurrent rotates (and revokes) of this destination.
		typ, err := gen.New(tx).DestTokDestLock(r.Context(), gen.DestTokDestLockParams{WorkspaceID: ws, ID: id})
		if err != nil {
			return err
		}
		if !destTokIsCRM(typ) {
			return errDestTokNotCRM
		}
		// Microsecond precision, as the column stores it, so the response matches what a later status read shows.
		res, err = destinbound.Rotate(r.Context(), tx, ws, id, c.User.ID, time.Now().UTC().Truncate(time.Microsecond))
		return err
	})
	if err != nil {
		h.fail(w, "rotate inbound token", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // the response carries the one-time plaintext token
	httpx.WriteJSON(w, http.StatusOK, destTokRotated{Token: res.Token, Prefix: res.Prefix,
		CreatedAt: naiveISO(res.CreatedAt), ExpiresAt: destTokTZ(res.ExpiresAt), Warning: destTokWarning})
}

type destTokStatus struct {
	Active     bool    `json:"active"`
	Expired    bool    `json:"expired"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  *string `json:"expires_at"`
	LastUsedAt *string `json:"last_used_at"`
	CreatedBy  *int32  `json:"created_by"`
}

func (h destTokens) status(w http.ResponseWriter, r *http.Request) {
	id, ok := h.destID(w, r)
	if !ok {
		return
	}
	c, _ := identity.WorkspaceFrom(r.Context())
	ws := c.WorkspaceID
	var st destinbound.TokenStatus
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		typ, err := gen.New(tx).DestTokDestType(r.Context(), gen.DestTokDestTypeParams{WorkspaceID: ws, ID: id})
		if err != nil {
			return err
		}
		if !destTokIsCRM(typ) {
			return errDestTokNotCRM
		}
		st, err = destinbound.Status(r.Context(), tx, ws, id, time.Now().UTC())
		return err
	})
	if err != nil {
		h.fail(w, "inbound token status", err)
		return
	}
	if !st.Has {
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"active": false})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, destTokStatus{Active: st.Active, Expired: st.Expired, Prefix: st.Prefix,
		CreatedAt: naiveISO(st.CreatedAt), ExpiresAt: destTokTZPtr(st.ExpiresAt), LastUsedAt: destTokTZPtr(st.LastUsedAt), CreatedBy: st.CreatedBy})
}

func (h destTokens) revoke(w http.ResponseWriter, r *http.Request) {
	id, ok := h.destID(w, r)
	if !ok {
		return
	}
	c, _ := identity.WorkspaceFrom(r.Context())
	ws := c.WorkspaceID
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		// No CRM-type check on revoke (as Python); the lock orders it against a concurrent rotate.
		if _, err := gen.New(tx).DestTokDestLock(r.Context(), gen.DestTokDestLockParams{WorkspaceID: ws, ID: id}); err != nil {
			return err
		}
		_, err := destinbound.Revoke(r.Context(), tx, ws, id, time.Now().UTC().Truncate(time.Microsecond))
		return err
	})
	if err != nil {
		h.fail(w, "revoke inbound token", err)
		return
	}
	w.Header().Set("Content-Type", "application/json") // FastAPI keeps it on a 204
	w.WriteHeader(http.StatusNoContent)
}

// ---- receipts ----

const destTokCursorLabel = "inbound receipt"

type destTokReceiptsPage struct {
	Receipts   []destinbound.Receipt `json:"receipts"`
	Limit      int                   `json:"limit"`
	Offset     *int                  `json:"offset"`
	HasMore    bool                  `json:"has_more"`
	NextCursor *string               `json:"next_cursor"`
}

func (h destTokens) receipts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset, errs := audAccountsQuery(q) // limit 1..200 (50), offset 0..100000 (0): the same declaration
	var cursor string
	if vs := q["cursor"]; len(vs) > 0 {
		cursor = vs[len(vs)-1]
		if utf8.RuneCountInString(cursor) > 1024 {
			errs = append(errs, httpx.FieldError{Type: "string_too_long", Loc: []any{"query", "cursor"}, Input: cursor,
				Msg: "String should have at most 1024 characters", Ctx: map[string]any{"max_length": 1024}})
		}
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	id, ok := h.destID(w, r)
	if !ok {
		return
	}
	c, _ := identity.WorkspaceFrom(r.Context())
	ws := c.WorkspaceID
	var rows []gen.DestTokReceiptsPageRow
	bad := false
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		qs := gen.New(tx)
		if _, err := qs.DestTokDestType(r.Context(), gen.DestTokDestTypeParams{WorkspaceID: ws, ID: id}); err != nil {
			return err
		}
		p := gen.DestTokReceiptsPageParams{WorkspaceID: ws, DestinationID: id, PageLimit: int32(limit + 1)}
		if cursor != "" {
			at, cid, ok := destTokDecodeCursor(cursor)
			if !ok {
				bad = true
				return nil
			}
			p.HasCursor, p.CursorID = true, cid
			p.CursorAt.Time, p.CursorAt.Valid = at, true
		} else {
			p.PageOffset = int32(offset)
		}
		var err error
		rows, err = qs.DestTokReceiptsPage(r.Context(), p)
		return err
	})
	if bad {
		httpx.Detail(w, http.StatusUnprocessableEntity, "invalid "+destTokCursorLabel+" cursor")
		return
	}
	if err != nil {
		h.fail(w, "list inbound receipts", err)
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	out := destTokReceiptsPage{Receipts: make([]destinbound.Receipt, 0, len(rows)), Limit: limit, HasMore: more}
	if cursor == "" {
		out.Offset = &offset
	}
	for _, row := range rows {
		rc := destinbound.Receipt{ID: row.ID, DestinationID: row.DestinationID, Provider: row.Provider, ExternalEventID: row.ExternalEventID,
			Status: row.Status, ConflictPolicy: row.ConflictPolicy, CreatedAt: row.CreatedAt.Time, AppliedFields: []string{}, IgnoredFields: []string{}}
		if row.ExternalRecordID.Valid {
			rc.ExternalRecordID = &row.ExternalRecordID.String
		}
		if row.LeadID.Valid {
			rc.LeadID = &row.LeadID.Int32
		}
		if row.Error.Valid {
			rc.Error = &row.Error.String
		}
		if err := json.Unmarshal(row.AppliedFields, &rc.AppliedFields); err != nil {
			h.internal(w, "decode receipt applied_fields", err)
			return
		}
		if err := json.Unmarshal(row.IgnoredFields, &rc.IgnoredFields); err != nil {
			h.internal(w, "decode receipt ignored_fields", err)
			return
		}
		out.Receipts = append(out.Receipts, rc)
	}
	if more {
		last := rows[len(rows)-1]
		nc := destTokEncodeCursor(last.CreatedAt.Time, last.ID)
		out.NextCursor = &nc
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// destTokEncodeCursor is _encode_history_cursor: urlsafe base64 (no padding) of json.dumps([iso, id], separators=(",", ":")).
func destTokEncodeCursor(createdAt time.Time, id string) string {
	var b bytes.Buffer
	b.WriteString(`["` + naiveISO(createdAt) + `",`)
	destTokPyString(&b, id)
	b.WriteByte(']')
	return strings.TrimRight(b64url(b.Bytes()), "=")
}

// destTokPyString writes s like json.dumps with ensure_ascii=True.
func destTokPyString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
const b64urlAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

func b64url(in []byte) string {
	var sb strings.Builder
	for i := 0; i < len(in); i += 3 {
		var n uint32
		rem := len(in) - i
		for j := 0; j < 3; j++ {
			n <<= 8
			if j < rem {
				n |= uint32(in[i+j])
			}
		}
		for j := 0; j < 4; j++ {
			if j <= rem {
				sb.WriteByte(b64urlAlphabet[(n>>(18-6*j))&63])
			} else {
				sb.WriteByte('=')
			}
		}
	}
	return sb.String()
}

// destTokB64Decode is base64.urlsafe_b64decode(s + "=" * (-len(s) % 4)) as CPython's non-strict a2b_base64 runs it:
// '-' and '_' become '+' and '/', characters outside the alphabet are skipped, padding past the first two characters of
// a quad ends the input, and a dangling quad is an error.
func destTokB64Decode(s string) ([]byte, bool) {
	s += strings.Repeat("=", (4-len(s)%4)%4)
	var out []byte
	quad, pads := 0, 0
	var left byte
	padded := false
loop:
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '-':
			ch = '+'
		case '_':
			ch = '/'
		}
		if ch == '=' {
			if quad >= 2 {
				pads++
				if quad+pads >= 4 {
					padded = true // the quad is already interpreted; the dangling-quad check is skipped
					break loop
				}
			}
			continue
		}
		v := strings.IndexByte(b64Alphabet, ch)
		if v < 0 {
			continue
		}
		pads = 0
		b := byte(v)
		switch quad {
		case 0:
			quad, left = 1, b
		case 1:
			quad = 2
			out = append(out, left<<2|b>>4)
			left = b & 0xf
		case 2:
			quad = 3
			out = append(out, left<<4|b>>2)
			left = b & 3
		default:
			quad = 0
			out = append(out, left<<6|b)
		}
	}
	if quad != 0 && !padded {
		return nil, false
	}
	return out, true
}

// destTokDecodeCursor is _decode_history_cursor: [iso-datetime, id-string]; a naive datetime is UTC.
func destTokDecodeCursor(cursor string) (time.Time, string, bool) {
	raw, ok := destTokB64Decode(cursor)
	if !ok || !utf8.Valid(raw) {
		return time.Time{}, "", false
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var v []json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil || len(v) != 2 {
		return time.Time{}, "", false
	}
	var iso, id string
	if json.Unmarshal(v[0], &iso) != nil || json.Unmarshal(v[1], &id) != nil {
		return time.Time{}, "", false
	}
	if strings.ContainsRune(id, 0) || !utf8.ValidString(id) {
		return time.Time{}, "", false
	}
	at, ok := destTokFromISO(iso)
	return at, id, ok
}

var (
	destTokISOExt   = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:.(\d{2})(?::(\d{2})(?::(\d{2})(?:[.,](\d+))?)?)?(Z|[+-]\d{2}(?::\d{2}(?::\d{2}(?:[.,]\d+)?)?)?)?)?$`)
	destTokISOBasic = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})(?:.(\d{2})(?:(\d{2})(?:(\d{2})(?:[.,](\d+))?)?)?(Z|[+-]\d{2}(?:\d{2}(?:\d{2}(?:[.,]\d+)?)?)?)?)?$`)
	destTokTZPat    = regexp.MustCompile(`^([+-])(\d{2}):?(\d{2})?:?(\d{2})?(?:[.,](\d+))?$`)
)

// destTokFromISO is datetime.fromisoformat for the calendar-date forms (Python 3.11+): YYYY-MM-DD or YYYYMMDD, an
// optional one-character separator and HH[:MM[:SS[.ffffff]]] with an optional Z or ±HH[:MM[:SS]] offset. Week dates and
// 24:00 are not accepted (documented deviation). A naive value is UTC, as the route does.
func destTokFromISO(s string) (time.Time, bool) {
	m := destTokISOExt.FindStringSubmatch(s)
	if m == nil {
		m = destTokISOBasic.FindStringSubmatch(s)
	}
	if m == nil {
		return time.Time{}, false
	}
	num := func(x string) int { n, _ := strconv.Atoi(x); return n }
	y, mo, d := num(m[1]), num(m[2]), num(m[3])
	h, mi, sec, nsec := num(m[4]), num(m[5]), num(m[6]), 0
	if m[7] != "" {
		f := (m[7] + "000000")[:6]
		nsec = num(f) * 1000
	}
	if y < 1 || mo < 1 || mo > 12 || d < 1 || h > 23 || mi > 59 || sec > 59 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(mo), d, h, mi, sec, nsec, time.UTC)
	if t.Day() != d { // day out of range for the month
		return time.Time{}, false
	}
	if tz := m[8]; tz != "" && tz != "Z" {
		z := destTokTZPat.FindStringSubmatch(tz)
		if z == nil {
			return time.Time{}, false
		}
		oh, om, os := num(z[2]), num(z[3]), num(z[4])
		if oh > 23 || om > 59 || os > 59 {
			return time.Time{}, false
		}
		off := time.Duration(oh)*time.Hour + time.Duration(om)*time.Minute + time.Duration(os)*time.Second
		if z[1] == "-" {
			off = -off
		}
		t = t.Add(-off)
	}
	return t, true
}
