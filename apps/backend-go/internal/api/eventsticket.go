package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// SSE tickets (S-L23). EventSource cannot send headers, so instead of the access JWT riding in
// ?token= (logged by proxies, kept in history) the client first calls POST /api/events/ticket
// with its bearer token and then opens GET /api/events?ticket=. A ticket lives 60 s, is bound to
// the user, the workspace and the minting token's amr, and is single-use.
//
// The ticket is HMAC-signed (key derived from the JWT secret), so any API replica can verify it;
// single use is enforced by recording its nonce: in Redis (SET NX, shared by all replicas) when
// configured, else in this process. Without Redis a ticket could be replayed once per other
// replica within its 60 s; without Redis the stream carries heartbeats only (decision D3).
const (
	eventsTicketTTL      = 60 * time.Second
	eventsTicketVersion  = "v1"
	invalidTicketDetail  = "Invalid ticket"
	queryTokenGoneDetail = "Query token no longer accepted; use POST /api/events/ticket"
	// defaultQueryTokenUntil ends the deprecation window of ?token= on /api/events when
	// SSE_QUERY_TOKEN_UNTIL is unset.
	defaultQueryTokenUntil = "2027-01-31T00:00:00Z"
)

// ticketNow is the ticket clock (a variable so tests can move it).
var ticketNow = time.Now

type ticketClaims struct {
	UID   int32    `json:"u"`
	WS    string   `json:"w"`
	AMR   []string `json:"a,omitempty"`
	Exp   int64    `json:"e"`
	Nonce string   `json:"n"`
}

// ticketer mints and redeems SSE tickets.
type ticketer struct {
	key   []byte
	redis *redis.Client

	mu   sync.Mutex
	used map[string]time.Time // nonce -> expiry (process-local single use)
}

func newTicketer(secret []byte, rc *redis.Client) *ticketer {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("opengtm/sse-ticket/" + eventsTicketVersion))
	return &ticketer{key: m.Sum(nil), redis: rc, used: map[string]time.Time{}}
}

func (t *ticketer) sign(payload string) string {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (t *ticketer) mint(uid int32, ws string, amr []string) (string, error) {
	n := make([]byte, 18)
	if _, err := rand.Read(n); err != nil {
		return "", err
	}
	raw, err := json.Marshal(ticketClaims{UID: uid, WS: ws, AMR: amr,
		Exp: ticketNow().Add(eventsTicketTTL).Unix(), Nonce: base64.RawURLEncoding.EncodeToString(n)})
	if err != nil {
		return "", err
	}
	payload := eventsTicketVersion + "." + base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + t.sign(payload), nil
}

var errBadTicket = errors.New("invalid ticket")

// redeem verifies the signature and expiry and consumes the nonce. errBadTicket covers a forged,
// expired or reused ticket alike; any other error is a store failure.
func (t *ticketer) redeem(ctx context.Context, tk string) (ticketClaims, error) {
	var c ticketClaims
	i := strings.LastIndexByte(tk, '.')
	if i < 0 || !strings.HasPrefix(tk, eventsTicketVersion+".") {
		return c, errBadTicket
	}
	payload, sig := tk[:i], tk[i+1:]
	if !hmac.Equal([]byte(sig), []byte(t.sign(payload))) {
		return c, errBadTicket
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(payload, eventsTicketVersion+"."))
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Nonce == "" || c.WS == "" || c.UID == 0 {
		return ticketClaims{}, errBadTicket
	}
	now := ticketNow()
	exp := time.Unix(c.Exp, 0)
	if !now.Before(exp) {
		return ticketClaims{}, errBadTicket
	}
	if t.redis != nil {
		ok, err := t.redis.SetNX(ctx, "opengtm:sse-ticket:"+c.Nonce, 1, exp.Sub(now)+time.Second).Result()
		if err != nil {
			return ticketClaims{}, err
		}
		if !ok {
			return ticketClaims{}, errBadTicket
		}
		return c, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for n, e := range t.used {
		if !now.Before(e) {
			delete(t.used, n)
		}
	}
	if _, seen := t.used[c.Nonce]; seen {
		return ticketClaims{}, errBadTicket
	}
	t.used[c.Nonce] = exp
	return c, nil
}

// queryTokenUntil parses SSE_QUERY_TOKEN_UNTIL (RFC 3339 or YYYY-MM-DD, UTC). Unset uses the
// default window end; a value that does not parse closes the window (fail closed).
func queryTokenUntil(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		v = defaultQueryTokenUntil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// ticket serves POST /api/events/ticket (Authenticate + Workspace: the workspace, membership and
// SSO enforcement are the regular ones).
func (h *eventsRoute) ticket(w http.ResponseWriter, r *http.Request) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	tk, err := h.tickets.mint(c.User.ID, c.WorkspaceID, c.Claims.AMR)
	if err != nil {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	noStore(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ticket": tk, "expires_in": int(eventsTicketTTL / time.Second)})
}
