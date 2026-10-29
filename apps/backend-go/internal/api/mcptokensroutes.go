package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

const (
	maxMCPBody      = 1 << 20
	mcpTokenPrefix  = "ycp_"
	mcpMaxNameChars = 200
	// Python's datetime overflows (OverflowError -> 500) past year 9999.
	mcpMaxTTLDays = 2_900_000
)

// allMCPCapabilities is services/mcp/auth.py:ALL_CAPABILITIES.
var allMCPCapabilities = map[string]bool{
	"leads:read": true, "leads:write": true, "sequences:enroll": true, "workbooks:write": true,
	"automations:write": true, "outreach:send": true, "leads:delete": true, "workbooks:delete": true,
}

// RegisterMCPTokens registers /api/mcp/tokens (workspace admin only).
func RegisterMCPTokens(r *httpx.Router, d Deps) {
	m := mcpRoutes{d}
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity), identity.RequireAdmin()}
	r.Handle("POST /api/mcp/tokens", http.HandlerFunc(m.create), mws...)
	r.Handle("GET /api/mcp/tokens", http.HandlerFunc(m.list), mws...)
	r.Handle("DELETE /api/mcp/tokens/{token_id}", http.HandlerFunc(m.revoke), mws...)
}

type mcpRoutes struct{ d Deps }

func (m mcpRoutes) fail(w http.ResponseWriter, what string) {
	l := m.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("mcp tokens: " + what + " failed")
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// hashMCPToken is services/mcp/auth.py:hash_token (sha256 hex of the plaintext).
func hashMCPToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// newMCPToken mints "ycp_" + 32 random bytes as unpadded base64url, and its 12-char prefix.
func newMCPToken() (raw, prefix string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = mcpTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return raw, raw[:12], nil
}

// pyISO renders an aware-UTC time like Python's datetime.isoformat().
func pyISO(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	s := u.Format("2006-01-02T15:04:05")
	if us := u.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	s += "+00:00"
	return &s
}

type mcpTokenOut struct {
	ID           string   `json:"id"`
	Prefix       string   `json:"prefix"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	WorkspaceID  string   `json:"workspace_id"`
	CreatedAt    *string  `json:"created_at"`
	ExpiresAt    *string  `json:"expires_at"`
	LastUsedAt   *string  `json:"last_used_at"`
	RevokedAt    *string  `json:"revoked_at"`
}

type mcpTokenCreated struct {
	mcpTokenOut
	Token string `json:"token"`
}

func toMCPOut(t gen.McpToken) mcpTokenOut {
	caps := []string{}
	_ = json.Unmarshal(t.Capabilities, &caps)
	if caps == nil {
		caps = []string{}
	}
	return mcpTokenOut{
		ID: t.ID, Prefix: t.Prefix, Name: t.Name, Capabilities: caps, WorkspaceID: t.WorkspaceID,
		CreatedAt: pyISO(t.CreatedAt), ExpiresAt: pyISO(t.ExpiresAt),
		LastUsedAt: pyISO(t.LastUsedAt), RevokedAt: pyISO(t.RevokedAt),
	}
}

// mcpFieldErr is a pydantic error entry; httpx.FieldError has no ctx member.
type mcpFieldErr struct {
	Type  string         `json:"type"`
	Loc   []any          `json:"loc"`
	Msg   string         `json:"msg"`
	Input any            `json:"input"`
	Ctx   map[string]any `json:"ctx,omitempty"`
}

type mcpCreateBody struct {
	name   string
	caps   []string
	ttl    int64
	hasTTL bool
	hugeTT bool // ttl_days beyond int64 range, positive
}

// parseCreate validates the body like pydantic's TokenCreate (lax mode).
func parseCreate(raw []byte) (mcpCreateBody, []mcpFieldErr) {
	out := mcpCreateBody{caps: []string{"leads:read"}}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, []mcpFieldErr{{Type: "missing", Loc: []any{"body"}, Msg: "Field required", Input: nil}}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body any
	if err := dec.Decode(&body); err != nil || dec.More() {
		return out, []mcpFieldErr{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return out, []mcpFieldErr{{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body}}
	}
	var errs []mcpFieldErr

	if v, present := obj["name"]; present {
		s, isStr := v.(string)
		switch {
		case !isStr:
			errs = append(errs, mcpFieldErr{Type: "string_type", Loc: []any{"body", "name"}, Msg: "Input should be a valid string", Input: v})
		case utf8.RuneCountInString(s) > mcpMaxNameChars:
			errs = append(errs, mcpFieldErr{Type: "string_too_long", Loc: []any{"body", "name"},
				Msg: "String should have at most 200 characters", Input: s, Ctx: map[string]any{"max_length": mcpMaxNameChars}})
		default:
			out.name = s
		}
	}

	if v, present := obj["capabilities"]; present {
		list, isList := v.([]any)
		if !isList {
			errs = append(errs, mcpFieldErr{Type: "list_type", Loc: []any{"body", "capabilities"}, Msg: "Input should be a valid list", Input: v})
		} else {
			caps := make([]string, 0, len(list))
			for i, el := range list {
				s, isStr := el.(string)
				if !isStr {
					errs = append(errs, mcpFieldErr{Type: "string_type", Loc: []any{"body", "capabilities", i}, Msg: "Input should be a valid string", Input: el})
					continue
				}
				caps = append(caps, s)
			}
			out.caps = caps
		}
	}

	if v, present := obj["ttl_days"]; present && v != nil {
		n, e := pyInt(v, []any{"body", "ttl_days"})
		if e != nil {
			errs = append(errs, *e)
		} else {
			out.hasTTL, out.ttl = true, n.v
			out.hugeTT = n.huge
		}
	}
	return out, errs
}

type pyIntVal struct {
	v    int64
	huge bool // positive integer beyond int64
}

// pyInt mirrors pydantic v2 lax int validation of a JSON value.
func pyInt(v any, loc []any) (pyIntVal, *mcpFieldErr) {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return pyIntVal{v: n}, nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err == nil && !strings.ContainsAny(s, ".eE") { // integer syntax beyond int64
			return pyIntVal{v: 0, huge: f > 0}, nil
		}
		if err == nil && f == math.Trunc(f) && math.Abs(f) < 1<<63 {
			return pyIntVal{v: int64(f)}, nil
		}
		return pyIntVal{}, &mcpFieldErr{Type: "int_from_float", Loc: loc, Msg: "Input should be a valid integer, got a number with a fractional part", Input: x}
	case string:
		t := strings.TrimSpace(x)
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return pyIntVal{v: n}, nil
		}
		return pyIntVal{}, &mcpFieldErr{Type: "int_parsing", Loc: loc, Msg: "Input should be a valid integer, unable to parse string as an integer", Input: x}
	}
	return pyIntVal{}, &mcpFieldErr{Type: "int_type", Loc: loc, Msg: "Input should be a valid integer", Input: v}
}

// pyListRepr renders a sorted []str like Python's repr(sorted(set)).
func pyListRepr(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyStrRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func pyStrRepr(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

func (m mcpRoutes) create(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	body, errs := parseCreate(raw)
	if len(errs) > 0 {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": errs})
		return
	}
	ctx := r.Context()
	c, ok := identity.WorkspaceFrom(ctx)
	if !ok {
		m.fail(w, "context")
		return
	}

	caps := make([]string, 0, len(body.caps))
	seen := map[string]bool{}
	var bad []string
	for _, s := range body.caps {
		if seen[s] {
			continue
		}
		seen[s] = true
		caps = append(caps, s)
		if !allMCPCapabilities[s] {
			bad = append(bad, s)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		httpx.Detail(w, http.StatusBadRequest, "unknown capabilities: "+pyListRepr(bad))
		return
	}

	ttl := int64(m.d.Config.MCPTokenTTLDays)
	if body.hasTTL {
		ttl = body.ttl
	}
	var expires pgtype.Timestamptz
	if body.hugeTT || ttl > mcpMaxTTLDays {
		m.fail(w, "ttl overflow") // Python raises OverflowError -> 500
		return
	}
	if ttl > 0 {
		expires = pgtype.Timestamptz{Time: time.Now().UTC().Add(time.Duration(ttl) * 24 * time.Hour), Valid: true}
	}

	plain, prefix, err := newMCPToken()
	if err != nil {
		m.fail(w, "mint")
		return
	}
	capsJSON, _ := json.Marshal(caps)
	var row gen.McpToken
	err = c.Tx(ctx, func(tx pgx.Tx) error {
		var e error
		row, e = gen.New(tx).InsertMCPToken(ctx, gen.InsertMCPTokenParams{
			TokenHash: hashMCPToken(plain), Prefix: prefix, Name: body.name, UserID: c.User.ID,
			WorkspaceID: c.WorkspaceID, Capabilities: capsJSON, ExpiresAt: expires, CreatedBy: c.User.ID,
		})
		return e
	})
	if err != nil {
		m.fail(w, "insert")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, mcpTokenCreated{mcpTokenOut: toMCPOut(row), Token: plain})
}

func (m mcpRoutes) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := identity.WorkspaceFrom(ctx)
	if !ok {
		m.fail(w, "context")
		return
	}
	var rows []gen.McpToken
	err := c.Tx(ctx, func(tx pgx.Tx) error {
		var e error
		rows, e = gen.New(tx).ListMCPTokens(ctx, c.WorkspaceID)
		return e
	})
	if err != nil {
		m.fail(w, "list")
		return
	}
	out := make([]mcpTokenOut, 0, len(rows))
	for _, t := range rows {
		out = append(out, toMCPOut(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (m mcpRoutes) revoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := identity.WorkspaceFrom(ctx)
	if !ok {
		m.fail(w, "context")
		return
	}
	id := r.PathValue("token_id")
	err := c.Tx(ctx, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if _, e := q.GetMCPToken(ctx, gen.GetMCPTokenParams{ID: id, WorkspaceID: c.WorkspaceID}); e != nil {
			return e
		}
		return q.RevokeMCPToken(ctx, gen.RevokeMCPTokenParams{ID: id, WorkspaceID: c.WorkspaceID})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Detail(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		m.fail(w, "revoke")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}{true, id})
}
