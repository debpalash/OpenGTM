//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destinbound"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Plan 10 Task 20: POST/GET/DELETE /api/audience-destinations/{id}/inbound-token and GET .../inbound-receipts against the
// recorded Python fixtures (testdata/contract/destinations/tok_*, receipts_*; raw tokens are "<redacted>" there) over the
// plan 10 seed. Nothing here depends on sequence state: every id is explicit text.

type tokEnv struct {
	audEnv
	logs *bytes.Buffer
}

const tokDestPath = "/api/audience-destinations/"

// tokLockedBuf is a goroutine-safe log sink.
type tokLockedBuf struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (b *tokLockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func newTokEnv(t *testing.T) tokEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	logs := &bytes.Buffer{}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, slog.New(slog.NewTextHandler(&tokLockedBuf{buf: logs}, nil))))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{},
		Log:            slog.New(slog.NewTextHandler(&tokLockedBuf{buf: logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Getenv:         func(string) string { return "" },
		Flags:          config.Flags{PGLeadStore: true, Audiences: config.AudienceSettings{GoDestRoutesEnabled: true}},
		AudienceEvents: nopEmitter{}}
	RegisterDestinations(r, d)
	e := audEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, r: r}
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.seed(t, "dana", pythonHash, true, "user", false)
	alice, bob, erin, dana := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin"), e.mustUserByName(t, "dana")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "viewer")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-alpha", dana.ID, "admin")
	e.member(t, "ws-beta", bob.ID, "owner")
	audPurge(t)
	t.Cleanup(func() { audPurge(t) })
	audLoadSeed(t)
	return tokEnv{audEnv: e, logs: logs}
}

// tokActor is who recorded a fixture (the recorder redacts its headers).
func tokActor(name string) (user, ws string) {
	switch {
	case strings.Contains(name, "no_auth"):
		return "", ""
	case strings.Contains(name, "editor"), strings.Contains(name, "by_erin"):
		return "erin", "ws-alpha"
	case strings.Contains(name, "viewer"):
		return "bob", "ws-alpha"
	case strings.Contains(name, "beta_own"), strings.Contains(name, "none_seeded_crm"):
		return "bob", "ws-beta"
	}
	return "alice", "ws-alpha"
}

// replay runs one recorded request (ids maps recorded generated ids to ours) and compares it.
func (e tokEnv) replay(t *testing.T, name string, ids map[string]string, opts ...contract.Option) (*httptest.ResponseRecorder, contract.Resp) {
	t.Helper()
	fx := contract.Load(t, "destinations/"+name)
	user, ws := tokActor(name)
	path := audIDRe.ReplaceAllStringFunc(fx.Request.Path, func(s string) string {
		if v, ok := ids[s]; ok {
			return v
		}
		return s
	})
	rec, got := e.call(user, ws, fx.Request.Method, path, "")
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
	return rec, got
}

func (e tokEnv) do2(who, ws, method, path string) (*httptest.ResponseRecorder, map[string]any) {
	rec, _ := e.call(who, ws, method, tokDestPath+path, "")
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec, m
}

// newCRM inserts a hubspot destination (a copy of the seeded one under a new id and name).
func tokNewCRM(t *testing.T, id string) {
	t.Helper()
	_, err := audOwner(t).Exec(context.Background(), `INSERT INTO audience_destinations
	  (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map, health_status, created_at, updated_at)
	  SELECT $1, workspace_id, audience_id, 'T20 ' || $1, destination_type, enabled, config, field_map, health_status, created_at, updated_at
	  FROM audience_destinations WHERE id = 'dst-act-hubspot'`, id)
	if err != nil {
		t.Fatal(err)
	}
}

func tokActive(t *testing.T, ws, dest string) int {
	t.Helper()
	return audOwnerInt(t, `SELECT count(*) FROM destination_inbound_tokens WHERE workspace_id=$1 AND destination_id=$2 AND revoked_at IS NULL`, ws, dest)
}

var (
	tokRawRe     = regexp.MustCompile(`^dsi_[A-Za-z0-9_-]{43}$`)
	tokNaiveRe   = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{6})?$`)
	tokAwareRe   = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{6})?\+00:00$`)
	tokWarningTx = "Copy this token now; it will not be shown again."
)

func tokString(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func TestRotateReturnsPlaintextOnce(t *testing.T) {
	e := newTokEnv(t)
	tokNewCRM(t, "t20-crm")
	rec, m := e.do2("alice", "ws-alpha", "POST", "t20-crm/inbound-token")
	if rec.Code != 200 {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	raw := tokString(m, "token")
	if !tokRawRe.MatchString(raw) || tokString(m, "prefix") != raw[:12] || tokString(m, "warning") != tokWarningTx {
		t.Fatalf("shape: %v", m)
	}
	if !tokNaiveRe.MatchString(tokString(m, "created_at")) || !tokAwareRe.MatchString(tokString(m, "expires_at")) {
		t.Fatalf("timestamps: created_at=%q expires_at=%q", m["created_at"], m["expires_at"])
	}
	exp, err := time.Parse(time.RFC3339Nano, tokString(m, "expires_at"))
	if d := time.Until(exp); err != nil || d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Fatalf("expires_at not ~90d: %v (%v)", exp, err)
	}
	// Key order is the Python dict's.
	if got := destKeysOrdered(t, rec.Body.Bytes()); strings.Join(got, ",") != "token,prefix,created_at,expires_at,warning" {
		t.Fatalf("key order: %v", got)
	}
	// Only the digest is stored; nothing else ever returns the token.
	var hash, prefix string
	if err := audOwner(t).QueryRow(context.Background(), `SELECT token_hash, prefix FROM destination_inbound_tokens WHERE destination_id='t20-crm' AND revoked_at IS NULL`).Scan(&hash, &prefix); err != nil {
		t.Fatal(err)
	}
	if hash != destinbound.HashToken(raw) || prefix != raw[:12] {
		t.Fatalf("stored hash/prefix wrong: %s %s", hash, prefix)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM destination_inbound_tokens WHERE token_hash = $1 OR prefix = $1 OR id = $1`, raw); n != 0 {
		t.Fatal("plaintext persisted")
	}
	_, st := e.do2("alice", "ws-alpha", "GET", "t20-crm/inbound-token")
	if b, _ := json.Marshal(st); strings.Contains(string(b), raw) {
		t.Fatalf("status leaked the token: %s", b)
	}
	// A second rotate returns a different token.
	_, m2 := e.do2("alice", "ws-alpha", "POST", "t20-crm/inbound-token")
	if tokString(m2, "token") == raw {
		t.Fatal("rotate returned the same token twice")
	}
}

func destKeysOrdered(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestRotateNonCRM409(t *testing.T) {
	e := newTokEnv(t)
	for _, id := range []string{"dst-act-webhook", "dst-act-meta_ads", "dst-act-slack"} {
		for _, method := range []string{"POST", "GET"} {
			rec, m := e.do2("alice", "ws-alpha", method, id+"/inbound-token")
			if rec.Code != 409 || tokString(m, "detail") != "Inbound sync tokens are only available for CRM destinations" {
				t.Fatalf("%s %s: %d %s", method, id, rec.Code, rec.Body)
			}
		}
		if n := audOwnerInt(t, `SELECT count(*) FROM destination_inbound_tokens WHERE destination_id=$1`, id); n != 0 {
			t.Fatalf("%s: a token row was created", id)
		}
	}
}

func TestRotateRevokesPrevious(t *testing.T) {
	e := newTokEnv(t)
	ctx := context.Background()
	if tokActive(t, "ws-alpha", "dst-act-hubspot") != 1 {
		t.Fatal("seed should hold one active hubspot token")
	}
	rec, m := e.do2("alice", "ws-alpha", "POST", "dst-act-hubspot/inbound-token")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if tokActive(t, "ws-alpha", "dst-act-hubspot") != 1 {
		t.Fatal("exactly one active token must remain")
	}
	// The seeded raw token no longer authenticates; the new one does.
	pool := e.pool
	seedRaw := tokSeedRaw(t, "hub_active")
	if _, err := destinbound.Resolve(ctx, pool, seedRaw, "dst-act-hubspot", time.Now()); err == nil {
		t.Fatal("previous token still resolves after rotate")
	}
	ws, err := destinbound.Resolve(ctx, pool, tokString(m, "token"), "dst-act-hubspot", time.Now())
	if err != nil || ws != "ws-alpha" {
		t.Fatalf("new token: %v %q", err, ws)
	}
	// An expired seeded token is replaced as well (tok_rotate_salesforce_expired_replaced).
	if rec, _ := e.do2("alice", "ws-alpha", "POST", "dst-act-salesforce/inbound-token"); rec.Code != 200 {
		t.Fatalf("salesforce rotate: %d", rec.Code)
	}
	if tokActive(t, "ws-alpha", "dst-act-salesforce") != 1 {
		t.Fatal("expired token should have been revoked, one new active")
	}
	// Another destination's and workspace's tokens are untouched.
	if tokActive(t, "ws-alpha", "dst-act-hubspot-off") != 1 || tokActive(t, "ws-beta", "dst-beta-crm") != 1 {
		t.Fatal("rotate revoked an unrelated token")
	}
}

func tokSeedRaw(t *testing.T, key string) string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/audiences/seed.json")
	if err != nil {
		t.Fatal(err)
	}
	var seed struct {
		Raw map[string]string `json:"raw_inbound_tokens"`
	}
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	return seed.Raw[key]
}

// TestConcurrentRotateLeavesOneActive: the destination row lock serializes rotates.
func TestConcurrentRotateLeavesOneActive(t *testing.T) {
	e := newTokEnv(t)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, _ := e.do2("alice", "ws-alpha", "POST", "dst-act-hubspot/inbound-token")
			codes[i] = rec.Code
		}()
	}
	wg.Wait()
	for _, c := range codes {
		if c != 200 {
			t.Fatalf("codes: %v", codes)
		}
	}
	if n := tokActive(t, "ws-alpha", "dst-act-hubspot"); n != 1 {
		t.Fatalf("%d active tokens after concurrent rotates, want 1", n)
	}
}

func TestStatusShapes(t *testing.T) {
	e := newTokEnv(t)
	tokNewCRM(t, "t20-crm")
	// none
	rec, _ := e.do2("alice", "ws-alpha", "GET", "t20-crm/inbound-token")
	if rec.Body.String() != `{"active":false}` {
		t.Fatalf("none: %s", rec.Body)
	}
	// active: key order of the Python dict
	rec, m := e.do2("alice", "ws-alpha", "GET", "dst-act-hubspot/inbound-token")
	if got := destKeysOrdered(t, rec.Body.Bytes()); strings.Join(got, ",") != "active,expired,prefix,created_at,expires_at,last_used_at,created_by" {
		t.Fatalf("key order: %v", got)
	}
	if m["active"] != true || m["expired"] != false || m["prefix"] != "dsi_hubA0001" || m["created_by"] != float64(1) {
		t.Fatalf("active: %v", m)
	}
	if !tokNaiveRe.MatchString(tokString(m, "created_at")) || !tokAwareRe.MatchString(tokString(m, "expires_at")) || !tokAwareRe.MatchString(tokString(m, "last_used_at")) {
		t.Fatalf("timestamp formats: %v", m)
	}
	// expired (seeded salesforce token)
	_, m = e.do2("alice", "ws-alpha", "GET", "dst-act-salesforce/inbound-token")
	if m["active"] != false || m["expired"] != true || m["last_used_at"] != nil {
		t.Fatalf("expired: %v", m)
	}
	// the newest unrevoked token wins; revoked ones never show
	_, m = e.do2("alice", "ws-alpha", "GET", "dst-act-hubspot-off/inbound-token")
	if m["prefix"] != "dsi_hubD0001" {
		t.Fatalf("off: %v", m)
	}
}

func TestRevoke204AndCallbackRejected(t *testing.T) {
	e := newTokEnv(t)
	ctx := context.Background()
	rec, m := e.do2("alice", "ws-alpha", "POST", "dst-act-hubspot/inbound-token")
	raw := tokString(m, "token")
	if _, err := destinbound.Resolve(ctx, e.pool, raw, "dst-act-hubspot", time.Now()); err != nil {
		t.Fatalf("fresh token should resolve: %v", err)
	}
	rec, _ = e.do2("alice", "ws-alpha", "DELETE", "dst-act-hubspot/inbound-token")
	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("revoke: %d %q", rec.Code, rec.Body)
	}
	if _, err := destinbound.Resolve(ctx, e.pool, raw, "dst-act-hubspot", time.Now()); err == nil {
		t.Fatal("revoked token still authenticates")
	}
	if tokActive(t, "ws-alpha", "dst-act-hubspot") != 0 {
		t.Fatal("active tokens remain")
	}
	if _, m = e.do2("alice", "ws-alpha", "GET", "dst-act-hubspot/inbound-token"); m["active"] != false || len(m) != 1 {
		t.Fatalf("after revoke: %v", m)
	}
	// idempotent, and no CRM-type check on revoke
	for _, id := range []string{"dst-act-hubspot", "dst-act-webhook"} {
		if rec, _ = e.do2("alice", "ws-alpha", "DELETE", id+"/inbound-token"); rec.Code != 204 {
			t.Fatalf("%s: %d", id, rec.Code)
		}
	}
}

func TestAdminRequired(t *testing.T) {
	e := newTokEnv(t)
	for _, who := range []string{"erin", "bob"} { // editor, viewer
		for _, c := range [][2]string{{"POST", "inbound-token"}, {"GET", "inbound-token"}, {"DELETE", "inbound-token"}} {
			rec, m := e.do2(who, "ws-alpha", c[0], "dst-act-hubspot/"+c[1])
			if rec.Code != 403 || tokString(m, "detail") != "Workspace permission denied: secrets.manage" {
				t.Fatalf("%s %s %s: %d %s", who, c[0], c[1], rec.Code, rec.Body)
			}
		}
	}
	if rec, _ := e.do2("", "", "POST", "dst-act-hubspot/inbound-token"); rec.Code != 401 {
		t.Fatalf("no auth: %d", rec.Code)
	}
	// an admin member passes; so does the owner
	for _, who := range []string{"dana", "alice"} {
		if rec, _ := e.do2(who, "ws-alpha", "GET", "dst-act-hubspot/inbound-token"); rec.Code != 200 {
			t.Fatalf("%s: %d", who, rec.Code)
		}
	}
	// a viewer may still read receipts (current_workspace)
	if rec, _ := e.do2("bob", "ws-alpha", "GET", "dst-act-hubspot/inbound-receipts"); rec.Code != 200 {
		t.Fatalf("viewer receipts: %d", rec.Code)
	}
	// the role check comes before the destination lookup: an editor on a foreign id is 403, not 404
	if rec, _ := e.do2("erin", "ws-alpha", "POST", "dst-beta-crm/inbound-token"); rec.Code != 403 {
		t.Fatalf("editor foreign: %d", rec.Code)
	}
}

func TestForeignDestination404(t *testing.T) {
	e := newTokEnv(t)
	for _, c := range [][2]string{{"POST", "inbound-token"}, {"GET", "inbound-token"}, {"DELETE", "inbound-token"}, {"GET", "inbound-receipts"}} {
		for _, id := range []string{"dst-beta-crm", "does-not-exist", "dst-act-hubspot%00x", "%FF"} {
			rec, m := e.do2("alice", "ws-alpha", c[0], id+"/"+c[1])
			if rec.Code != 404 || tokString(m, "detail") != "Destination not found" {
				t.Fatalf("%s %s %s: %d %s", c[0], id, c[1], rec.Code, rec.Body)
			}
		}
	}
	// the foreign token was not touched
	if tokActive(t, "ws-beta", "dst-beta-crm") != 1 {
		t.Fatal("foreign workspace token changed")
	}
	// bob owns ws-beta and sees its own
	if rec, _ := e.do2("bob", "ws-beta", "POST", "dst-beta-crm/inbound-token"); rec.Code != 200 {
		t.Fatalf("own beta rotate: %d", rec.Code)
	}
}

// TestTokenNeverLoggedOrAudited: the raw token appears only in the rotate response body.
func TestTokenNeverLoggedOrAudited(t *testing.T) {
	e := newTokEnv(t)
	_, m := e.do2("alice", "ws-alpha", "POST", "dst-act-hubspot/inbound-token")
	raw := tokString(m, "token")
	if raw == "" {
		t.Fatal("no token")
	}
	e.do2("alice", "ws-alpha", "GET", "dst-act-hubspot/inbound-token")
	e.do2("alice", "ws-alpha", "DELETE", "dst-act-hubspot/inbound-token")
	// failures too
	e.do2("alice", "ws-alpha", "POST", "dst-act-webhook/inbound-token")
	time.Sleep(200 * time.Millisecond) // the audit row is written after the response
	if strings.Contains(e.logs.String(), raw) || strings.Contains(e.logs.String(), destinbound.HashToken(raw)) {
		t.Fatal("token or hash in the logs")
	}
	c := audOwner(t)
	var dump string
	if err := c.QueryRow(context.Background(), `SELECT coalesce(string_agg(to_jsonb(a)::text, E'\n'), '') FROM governance_audit_events a`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, raw) || strings.Contains(dump, destinbound.HashToken(raw)) {
		t.Fatal("token or hash in the audit log")
	}
	var n int
	if err := c.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE payload::text LIKE '%'||$1||'%'`, raw).Scan(&n); err != nil || n != 0 {
		t.Fatalf("token in jobs: %d %v", n, err)
	}
}

// TestTokenFixturesReplay replays every recorded tok_* case in the recorded order, mapping the created destination.
func TestTokenFixturesReplay(t *testing.T) {
	e := newTokEnv(t)
	crm := "00000000-0000-4000-8000-000000002081" // the recorded id of the destination the recording created
	tokNewCRM(t, crm)
	gen := []string{"$.token", "$.prefix", "$.created_at", "$.expires_at", "$.last_used_at"}
	vol := func(extra ...string) contract.Option { return contract.IgnorePaths(append(gen, extra...)...) }

	// reads over the seed alone; last_used_at was stamped by the recording's callbacks
	for _, n := range []string{"tok_status_active", "tok_status_expired", "tok_status_none_seeded_crm"} {
		e.replay(t, n, nil, contract.IgnorePaths("$.last_used_at"))
	}
	for _, n := range []string{"tok_status_non_crm_409", "tok_status_unknown_404", "tok_status_foreign_404", "tok_status_editor_403",
		"tok_status_viewer_403", "tok_status_no_auth", "tok_status_none_new_crm"} {
		e.replay(t, n, nil)
	}
	e.replay(t, "tok_rotate_first", nil, vol())
	e.replay(t, "tok_status_after_first_rotate", nil, vol())
	e.replay(t, "tok_rotate_hub", nil, vol())
	e.replay(t, "tok_status_after_rotate", nil, vol())
	e.replay(t, "tok_rotate_salesforce_expired_replaced", nil, vol())
	for _, n := range []string{"tok_rotate_non_crm_409", "tok_rotate_unknown_404", "tok_rotate_foreign_404", "tok_rotate_editor_403", "tok_rotate_no_auth",
		"tok_revoke_editor_403", "tok_revoke_no_auth", "tok_revoke_unknown_404", "tok_revoke_foreign_404"} {
		e.replay(t, n, nil)
	}
	for _, n := range []string{"tok_revoke", "tok_status_after_revoke", "tok_revoke_again", "tok_revoke_non_crm"} {
		e.replay(t, n, nil)
	}
}

// TestReceiptsCursorMatchesFixtures replays the recorded receipt listings over the seed.
func TestReceiptsCursorMatchesFixtures(t *testing.T) {
	e := newTokEnv(t)
	var names []string
	for _, n := range tokFixtureNames(t, "receipts_") {
		if n == "receipts_after_callbacks" { // needs the callbacks of Task 21
			continue
		}
		names = append(names, n)
	}
	if len(names) < 18 {
		t.Fatalf("expected the recorded receipt fixtures, found %d", len(names))
	}
	for _, n := range names {
		e.replay(t, n, nil)
	}
}

func TestReceiptsPagingCapsAndTenancy(t *testing.T) {
	e := newTokEnv(t)
	ctx := context.Background()
	c := audOwner(t)
	// 205 receipts for one destination (all one created_at: the id breaks ties), plus a lookalike row in another workspace.
	if _, err := c.Exec(ctx, `INSERT INTO destination_inbound_receipts (id, workspace_id, destination_id, provider, external_event_id, status,
	  conflict_policy, applied_fields, ignored_fields, payload_fingerprint, created_at)
	  SELECT 't20-r-' || lpad(g::text, 4, '0'), 'ws-alpha', 'dst-act-salesforce', 'salesforce', 't20-e-' || g, 'applied', 'fill_missing',
	         '[]', '[]', repeat('a', 64), TIMESTAMP '2026-09-01 10:00:00' FROM generate_series(1, 205) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `INSERT INTO destination_inbound_receipts (id, workspace_id, destination_id, provider, external_event_id, status,
	  conflict_policy, applied_fields, ignored_fields, payload_fingerprint, created_at)
	  VALUES ('t20-foreign', 'ws-beta', 'dst-act-salesforce', 'salesforce', 'x', 'applied', 'fill_missing', '[]', '[]', repeat('a', 64), TIMESTAMP '2026-09-02 10:00:00')`); err != nil {
		t.Fatal(err)
	}
	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		path := "dst-act-salesforce/inbound-receipts?limit=200"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec, m := e.do2("alice", "ws-alpha", "GET", path)
		if rec.Code != 200 {
			t.Fatalf("page %d: %d %s", page, rec.Code, rec.Body)
		}
		if got := destKeysOrdered(t, rec.Body.Bytes()); strings.Join(got, ",") != "receipts,limit,offset,has_more,next_cursor" {
			t.Fatalf("key order: %v", got)
		}
		for _, r := range m["receipts"].([]any) {
			seen = append(seen, r.(map[string]any)["id"].(string))
		}
		if page == 0 && m["offset"] != float64(0) {
			t.Fatalf("offset: %v", m["offset"])
		}
		if cursor != "" && m["offset"] != nil {
			t.Fatalf("offset with cursor must be null: %v", m["offset"])
		}
		if m["has_more"] != true {
			break
		}
		cursor = m["next_cursor"].(string)
	}
	if len(seen) < 205 {
		t.Fatalf("saw %d receipts", len(seen))
	}
	for i, id := range seen {
		if id == "t20-foreign" {
			t.Fatal("another workspace's receipt leaked")
		}
		if strings.HasPrefix(id, "t20-r-") && i > 0 && strings.HasPrefix(seen[i-1], "t20-r-") && seen[i-1] < id {
			t.Fatalf("not ordered id DESC: %s then %s", seen[i-1], id)
		}
	}
	// the cursor is base64url(JSON [created_at isoformat, id]) without padding
	_, m := e.do2("alice", "ws-alpha", "GET", "dst-act-salesforce/inbound-receipts?limit=1")
	raw, err := base64.RawURLEncoding.DecodeString(m["next_cursor"].(string))
	if err != nil || string(raw) != `["2026-09-01T10:00:00","t20-r-0205"]` {
		t.Fatalf("cursor %q %v", raw, err)
	}
	// caps
	for _, q := range []string{"limit=0", "limit=201", "offset=-1", "offset=100001", "limit=x", "cursor=" + strings.Repeat("A", 1025)} {
		if rec, _ := e.do2("alice", "ws-alpha", "GET", "dst-act-salesforce/inbound-receipts?"+q); rec.Code != 422 {
			t.Fatalf("%s: %d", q, rec.Code)
		}
	}
	// a cursor beats the offset
	_, a := e.do2("alice", "ws-alpha", "GET", "dst-act-salesforce/inbound-receipts?limit=2&offset=50&cursor="+m["next_cursor"].(string))
	if a["receipts"].([]any)[0].(map[string]any)["id"] != "t20-r-0204" {
		t.Fatalf("cursor+offset: %v", a["receipts"])
	}
	// cursor variants Python's fromisoformat/json accept or reject
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for cur, want := range map[string]int{
		enc(`["2026-09-01T10:00:00+00:00","t20-r-0003"]`): 200,
		enc(`["2026-09-01","t20-r-0003"]`):                200,
		enc(`["2026-09-01 10:00:00","t20-r-0003"]`):       200,
		enc(`["nope","t20-r-0003"]`):                      422,
		enc(`["2026-09-01T10:00:00"]`):                    422,
		enc(`["2026-09-01T10:00:00",3]`):                  422,
		enc(`{"a":1}`):                                    422,
		enc(`[1,"x"]`):                                    422,
		"!!!!":                                            422,
	} {
		rec, _ := e.do2("alice", "ws-alpha", "GET", "dst-act-salesforce/inbound-receipts?cursor="+cur)
		if rec.Code != want {
			t.Fatalf("cursor %s: %d want %d (%s)", cur, rec.Code, want, rec.Body)
		}
	}
	// destination of this workspace with an id that exists elsewhere only: listing is workspace-scoped
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM destination_inbound_receipts WHERE id='t20-foreign'`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("RLS shows the foreign row")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func tokFixtureNames(t *testing.T, prefix string) []string {
	t.Helper()
	ents, err := os.ReadDir("../../testdata/contract/destinations")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ent := range ents {
		if n := strings.TrimSuffix(ent.Name(), ".json"); strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}
