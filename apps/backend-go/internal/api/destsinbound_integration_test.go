//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destinbound"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Plan 10 Task 21: the public CRM inbound callback POST /api/audience-destinations/inbound/{destination_id}. Authenticated only
// by the destination-bound inbound token; replayed against the recorded Python fixtures (testdata/contract/destinations/cb_*).
// Seeds use explicit ids (audLoadSeed advances the sequences); no assertion depends on a sequence value.

const inbPrefix = destPrefix + "/inbound/"

type inbEnv struct {
	e    destEnv
	pool *pgxpool.Pool
	h    http.Handler
	logs *bytes.Buffer
	tok  map[string]string
}

type inbOpts struct {
	getenv map[string]string
	wrap   func(*pgxpool.Pool) *pgxpool.Pool // the pool the routes run on (default: the test app pool)
}

func newInbEnv(t *testing.T, opts ...inbOpts) inbEnv {
	t.Helper()
	var o inbOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	e := newDestEnv(t)
	pool := e.pool
	if o.wrap != nil {
		pool = o.wrap(pool)
	}
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "t21-master", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: e.st, Config: config.APIConfig{}, Envelope: envl,
		Getenv: func(k string) string { return o.getenv[k] },
		Log:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Flags: config.Flags{PGLeadStore: true, Audiences: config.AudienceSettings{GoAudienceRoutesEnabled: true,
			GoDestRoutesEnabled: true, RefreshPageSize: 500}},
		AudienceEvents: nopEmitter{}}
	RegisterDestinations(r, d)
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
	return inbEnv{e: e, pool: pool, h: r.Handler(), logs: logs, tok: seed.Raw}
}

const inbPeer = "198.51.100.7:4000"

// do sends one callback. auth is the whole Authorization header value ("" sends none).
func (v inbEnv) do(destID, auth, body string, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", inbPrefix+destID, strings.NewReader(body))
	req.RemoteAddr = inbPeer
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	v.h.ServeHTTP(rec, req)
	return rec
}

func (v inbEnv) resp(rec *httptest.ResponseRecorder) contract.Resp {
	hdr := map[string]string{}
	for _, k := range []string{"Content-Type", "WWW-Authenticate"} {
		if s := rec.Header().Get(k); s != "" {
			hdr[strings.ToLower(k)] = s
		}
	}
	return contract.Resp{Status: rec.Code, Headers: hdr, Body: json.RawMessage(rec.Body.Bytes())}
}

func (v inbEnv) bearer(key string) string { return "Bearer " + v.tok[key] }

// authFor is the Authorization header the recording used for a fixture (the fixtures redact it).
func (v inbEnv) authFor(name, destID string) string {
	switch name {
	case "cb_no_auth_header":
		return ""
	case "cb_bad_token", "cb_invalid_body_beats_bad_token_422":
		return "Bearer dsi_bogus_not_a_real_token"
	case "cb_basic_scheme":
		return "Basic " + v.tok["hub_active"]
	case "cb_bearer_empty":
		return "Bearer "
	case "cb_lowercase_bearer":
		return "bearer " + v.tok["hub_active"]
	case "cb_revoked_token":
		return v.bearer("hub_revoked")
	case "cb_expired_token":
		return v.bearer("sf_expired")
	case "cb_other_workspace_token":
		return v.bearer("beta_active")
	case "cb_disabled_destination_404":
		return v.bearer("hubdis_active")
	}
	if destID == "dst-beta-crm" {
		return v.bearer("beta_active")
	}
	return v.bearer("hub_active") // includes cb_foreign_destination_token (a hubspot token on the salesforce path)
}

func inbEvent(id string) string {
	return fmt.Sprintf(`{"external_event_id":%q,"fields":{}}`, id)
}

func (v inbEnv) receiptCount(t *testing.T, ws string) int {
	t.Helper()
	return destOwnerInt(t, `SELECT count(*) FROM destination_inbound_receipts WHERE workspace_id = $1`, ws)
}

// TestInboundMatchesFixtures replays every recorded callback (valid, replay, unmatched, no_change, coercions, the 422 and the
// refusals) in recorded order and compares status, challenge header and body (volatile id and created_at aside).
func TestInboundMatchesFixtures(t *testing.T) {
	v := newInbEnv(t)
	ents, err := os.ReadDir("../../testdata/contract/destinations")
	if err != nil {
		t.Fatal(err)
	}
	type item struct {
		name    string
		fx      contract.Fixture
		created string
		replay  bool
	}
	var items []item
	for _, ent := range ents {
		n := strings.TrimSuffix(ent.Name(), ".json")
		if !strings.HasPrefix(n, "cb_") || strings.HasPrefix(n, "cb_after_") || n == "cb_deleted_destination_token_401" {
			continue
		}
		fx := contract.Load(t, "destinations/"+n)
		it := item{name: n, fx: fx}
		var b struct {
			CreatedAt string `json:"created_at"`
			Replay    bool   `json:"replay"`
		}
		_ = json.Unmarshal(fx.Response.Body, &b)
		it.created, it.replay = b.CreatedAt, b.Replay
		items = append(items, it)
	}
	if len(items) < 40 {
		t.Fatalf("only %d callback fixtures", len(items))
	}
	// Recorded order: state-changing callbacks by the receipt they created (a replay after the original); the rest are stateless.
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.created != b.created {
			return a.created < b.created
		}
		return !a.replay && b.replay
	})
	firstID := map[string]string{}
	for _, it := range items {
		it := it
		destID := it.fx.Request.Path[strings.LastIndex(it.fx.Request.Path, "/")+1:]
		body := string(it.fx.Request.Body)
		rec := v.do(destID, v.authFor(it.name, destID), body)
		opts := []contract.Option{}
		if it.fx.Response.Status == 200 {
			opts = append(opts, contract.IgnorePaths("$.id"), contract.IgnoreShapeOf("$.created_at"))
		}
		t.Run(it.name, func(t *testing.T) {
			contract.AssertEqual(t, v.resp(rec), it.fx.Response, opts...)
		})
		if it.fx.Response.Status == 200 {
			var got struct {
				ID, ExternalEventID string
				Replay              bool
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &struct {
				ID              *string `json:"id"`
				ExternalEventID *string `json:"external_event_id"`
			}{&got.ID, &got.ExternalEventID})
			key := destID + "|" + got.ExternalEventID
			if it.replay {
				if firstID[key] != got.ID {
					t.Errorf("%s: replay id %q, original %q", it.name, got.ID, firstID[key])
				}
			} else {
				firstID[key] = got.ID
			}
		}
	}
}

// TestInboundResponseKeyOrder pins the receipt key order with "replay" last (Python's dict order).
func TestInboundResponseKeyOrder(t *testing.T) {
	v := newInbEnv(t)
	rec := v.do("dst-act-hubspot", v.bearer("hub_active"), `{"external_event_id":"evt-ko-1","external_record_id":"hs-5","lead_id":5,"fields":{"jobtitle":"CTO"}}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	raw, err := os.ReadFile("../../testdata/contract/destinations/response_keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var shapes struct {
		Shapes map[string][]string `json:"shapes"`
	}
	_ = json.Unmarshal(raw, &shapes)
	if got, want := destKeys(t, rec.Body.Bytes()), shapes.Shapes["inbound_callback_response"]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys %v, want %v", got, want)
	}
}

// TestInboundAuthFailuresIdentical401WithChallenge: unknown destination, unknown token, revoked, expired, wrong scheme, no
// header, another destination's token and another workspace's token are one indistinguishable 401 with the Bearer challenge.
func TestInboundAuthFailuresIdentical401WithChallenge(t *testing.T) {
	v := newInbEnv(t)
	body := inbEvent("evt-auth")
	cases := map[string]*httptest.ResponseRecorder{
		"unknown dest":    v.do("does-not-exist", v.bearer("hub_active"), body),
		"bad token":       v.do("dst-act-hubspot", "Bearer nope", body),
		"revoked":         v.do("dst-act-hubspot", v.bearer("hub_revoked"), body),
		"expired":         v.do("dst-act-salesforce", v.bearer("sf_expired"), body),
		"basic":           v.do("dst-act-hubspot", "Basic "+v.tok["hub_active"], body),
		"empty":           v.do("dst-act-hubspot", "Bearer ", body),
		"none":            v.do("dst-act-hubspot", "", body),
		"other dest":      v.do("dst-act-salesforce", v.bearer("hub_active"), body),
		"other workspace": v.do("dst-act-hubspot", v.bearer("beta_active"), body),
		"beta dest alpha": v.do("dst-beta-crm", v.bearer("hub_active"), body),
	}
	for name, rec := range cases {
		if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" ||
			strings.TrimSpace(rec.Body.String()) != `{"detail":"invalid or revoked destination token"}` {
			t.Errorf("%s: %d %q hdr=%q", name, rec.Code, rec.Body, rec.Header().Get("WWW-Authenticate"))
		}
	}
	if destOwnerInt(t, `SELECT count(*) FROM destination_inbound_receipts WHERE external_event_id = 'evt-auth'`) != 0 {
		t.Fatal("a refused callback stored a receipt")
	}
}

func TestInboundForeignDestinationToken401(t *testing.T) {
	v := newInbEnv(t)
	// hub token (dst-act-hubspot) on the salesforce destination, and the salesforce one with an unexpired copy of itself elsewhere.
	rec := v.do("dst-act-salesforce", v.bearer("hub_active"), inbEvent("evt-foreign"))
	if rec.Code != 401 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := destOwnerInt(t, `SELECT count(*) FROM destination_inbound_tokens WHERE id = 'tok-hub-active' AND last_used_at IS NOT NULL AND last_used_at > now() - interval '1 minute'`); n != 0 {
		t.Fatal("a refused token was stamped as used")
	}
}

// TestInboundDisabledOrNonCRM404: a valid token for a disabled CRM destination, or for a non-CRM destination, is 404 with
// Python's text (and writes nothing).
func TestInboundDisabledOrNonCRM404(t *testing.T) {
	v := newInbEnv(t)
	ownerExecT(t, `INSERT INTO destination_inbound_tokens (id, workspace_id, destination_id, token_hash, prefix, created_by, created_at, expires_at)
		VALUES ('tok-t21-webhook','ws-alpha','dst-act-webhook',$1,'dsi_t21hook1',1,now(),now() + interval '1 day')`, destinbound.HashToken("dsi_t21hook1_raw"))
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"disabled": v.do("dst-act-hubspot-off", v.bearer("hubdis_active"), inbEvent("evt-off")),
		"webhook":  v.do("dst-act-webhook", "Bearer dsi_t21hook1_raw", inbEvent("evt-wh")),
	} {
		if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Active CRM destination not found"}` {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if destOwnerInt(t, `SELECT count(*) FROM destination_inbound_receipts WHERE external_event_id IN ('evt-off','evt-wh')`) != 0 {
		t.Fatal("404 stored a receipt")
	}
}

func ownerExecT(t *testing.T, sql string, args ...any) {
	t.Helper()
	c := audOwner(t)
	if _, err := c.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// TestInboundBody422Shapes: FastAPI/pydantic error shapes, and validation runs before authentication (as in Python).
func TestInboundBody422Shapes(t *testing.T) {
	v := newInbEnv(t)
	for name, tc := range map[string]struct{ body, want string }{
		"empty body":    {"", `{"detail":[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]}`},
		"invalid json":  {`{"external_event_id":`, `{"detail":[{"type":"json_invalid","loc":["body",20],"msg":"JSON decode error","input":{},"ctx":{"error":"Expecting value"}}]}`},
		"array body":    {`[1]`, `{"detail":[{"type":"model_attributes_type","loc":["body"],"msg":"Input should be a valid dictionary or object to extract fields from","input":[1]}]}`},
		"event not str": {`{"external_event_id":5}`, `{"detail":[{"type":"string_type","loc":["body","external_event_id"],"msg":"Input should be a valid string","input":5}]}`},
		"event null":    {`{"external_event_id":null}`, `{"detail":[{"type":"string_type","loc":["body","external_event_id"],"msg":"Input should be a valid string","input":null}]}`},
		"all errors in model order": {`{"external_record_id":7,"lead_id":0,"fields":null}`,
			`{"detail":[{"type":"missing","loc":["body","external_event_id"],"msg":"Field required","input":{"external_record_id":7,"lead_id":0,"fields":null}},` +
				`{"type":"string_type","loc":["body","external_record_id"],"msg":"Input should be a valid string","input":7},` +
				`{"type":"greater_than_equal","loc":["body","lead_id"],"msg":"Input should be greater than or equal to 1","input":0,"ctx":{"ge":1}},` +
				`{"type":"dict_type","loc":["body","fields"],"msg":"Input should be a valid dictionary","input":null}]}`},
	} {
		rec := v.do("dst-act-hubspot", "Bearer nope", tc.body)
		if rec.Code != 422 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			continue
		}
		var got, want any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		_ = json.Unmarshal([]byte(tc.want), &want)
		// the recorded ctx.error text of json_invalid is parser-specific: only its presence is compared
		if name == "invalid json" {
			if !strings.Contains(rec.Body.String(), `"json_invalid"`) || !strings.Contains(rec.Body.String(), `"JSON decode error"`) {
				t.Errorf("%s: %s", name, rec.Body)
			}
			continue
		}
		if toJSON(got) != toJSON(want) {
			t.Errorf("%s:\n got %s\nwant %s", name, rec.Body, tc.want)
		}
	}
}

// TestInboundBearerCaseInsensitive: the scheme is case-insensitive and the token is trimmed, like Python's
// authorization[7:].strip().
func TestInboundBearerCaseInsensitive(t *testing.T) {
	v := newInbEnv(t)
	for i, auth := range []string{"bearer " + v.tok["hub_active"], "BEARER " + v.tok["hub_active"], "bEaReR   " + v.tok["hub_active"] + "  ",
		"Bearer\t" + v.tok["hub_active"]} {
		rec := v.do("dst-act-hubspot", auth, inbEvent(fmt.Sprintf("evt-case-%d", i)))
		want := 200
		if strings.HasPrefix(auth, "Bearer\t") {
			want = 401 // Python requires the literal space after the scheme
		}
		if rec.Code != want {
			t.Errorf("%q: %d %s", auth, rec.Code, rec.Body)
		}
	}
}

// TestInboundRateLimited: 120 requests per minute per client IP (spent before the body is read), per verified token, 429 body
// as the shared limiter; the trusted-proxy client IP is used; junk tokens sharing a real token's prefix never spend its budget.
func TestInboundRateLimited(t *testing.T) {
	v := newInbEnv(t)
	for i := 0; i < 120; i++ {
		if rec := v.do("dst-act-hubspot", "Bearer junk", inbEvent("x")); rec.Code != 401 {
			t.Fatalf("request %d: %d", i+1, rec.Code)
		}
	}
	rec := v.do("dst-act-hubspot", v.bearer("hub_active"), inbEvent("evt-rl-0"))
	if rec.Code != 429 || strings.TrimSpace(rec.Body.String()) != `{"error":"Rate limit exceeded: 120 per 1 minute"}` {
		t.Fatalf("121st from one IP: %d %s", rec.Code, rec.Body)
	}
	// another client is unaffected; an untrusted peer cannot choose its bucket with X-Real-IP or X-Forwarded-For
	other := func(r *http.Request) { r.RemoteAddr = "198.51.100.99:1" }
	if rec := v.do("dst-act-hubspot", v.bearer("hub_active"), inbEvent("evt-rl-1"), other); rec.Code != 200 {
		t.Fatalf("other client: %d %s", rec.Code, rec.Body)
	}
	spoof := func(r *http.Request) {
		r.Header.Set("X-Real-IP", "203.0.113.5")
		r.Header.Set("X-Forwarded-For", "203.0.113.6")
	}
	if rec := v.do("dst-act-hubspot", v.bearer("hub_active"), inbEvent("evt-rl-2"), spoof); rec.Code != 429 {
		t.Fatalf("spoofed headers from the limited peer: %d", rec.Code)
	}
	// behind a trusted proxy (loopback) the X-Real-IP is the client
	proxied := func(ip string) func(*http.Request) {
		return func(r *http.Request) { r.RemoteAddr = "127.0.0.1:1"; r.Header.Set("X-Real-IP", ip) }
	}
	if rec := v.do("dst-act-hubspot", v.bearer("hub_active"), inbEvent("evt-rl-3"), proxied("203.0.113.50")); rec.Code != 200 {
		t.Fatalf("proxied client: %d %s", rec.Code, rec.Body)
	}
}

func TestInboundTokenBudgetPerVerifiedToken(t *testing.T) {
	v := newInbEnv(t)
	prefix := v.tok["hub_active"][:12]
	// 130 junk tokens with the real token's prefix, each from its own IP: the real token's budget is untouched
	for i := 0; i < 130; i++ {
		ip := fmt.Sprintf("192.0.2.%d:1", i+1)
		rec := v.do("dst-act-hubspot", "Bearer "+prefix+fmt.Sprintf("junk%d", i), inbEvent("x"), func(r *http.Request) { r.RemoteAddr = ip })
		if rec.Code != 401 {
			t.Fatalf("junk %d: %d", i, rec.Code)
		}
	}
	// the verified token: 120 per minute across any number of IPs, the 121st is 429
	for i := 0; i < 120; i++ {
		ip := fmt.Sprintf("10.9.%d.%d:1", i/200, i%200+1)
		rec := v.do("dst-act-hubspot", v.bearer("hub_active"), inbEvent("evt-tb"), func(r *http.Request) { r.RemoteAddr = ip })
		if rec.Code != 200 {
			t.Fatalf("verified %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := v.do("dst-act-hubspot", v.bearer("hub_active"), inbEvent("evt-tb"), func(r *http.Request) { r.RemoteAddr = "10.99.0.1:1" })
	if rec.Code != 429 || strings.TrimSpace(rec.Body.String()) != `{"error":"Rate limit exceeded: 120 per 1 minute"}` {
		t.Fatalf("121st verified request: %d %s", rec.Code, rec.Body)
	}
	// another token is not affected
	if rec := v.do("dst-beta-crm", v.bearer("beta_active"), inbEvent("evt-tb-b")); rec.Code != 200 {
		t.Fatalf("beta token: %d %s", rec.Code, rec.Body)
	}
}

// TestInboundBodyCap413: more than 1 MiB is refused before parsing; exactly 1 MiB is accepted.
func TestInboundBodyCap413(t *testing.T) {
	v := newInbEnv(t)
	ok := inbEvent("evt-cap")
	exact := ok + strings.Repeat(" ", 1<<20-len(ok))
	if rec := v.do("dst-act-hubspot", v.bearer("hub_active"), exact); rec.Code != 200 {
		t.Fatalf("1 MiB body: %d %.200s", rec.Code, rec.Body)
	}
	big := ok + strings.Repeat(" ", 1<<20-len(ok)+1)
	for _, auth := range []string{v.bearer("hub_active"), "Bearer nope", ""} {
		rec := v.do("dst-act-hubspot", auth, big)
		if rec.Code != 413 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Request body too large"}` {
			t.Errorf("1 MiB + 1 (%q): %d %.200s", auth, rec.Code, rec.Body)
		}
	}
}

// TestInboundConcurrentSameEventOneReceipt: parallel callbacks of one event id leave one receipt; exactly one is not a replay.
func TestInboundConcurrentSameEventOneReceipt(t *testing.T) {
	v := newInbEnv(t)
	const n = 12
	body := `{"external_event_id":"evt-conc","external_record_id":"hs-5","lead_id":5,"fields":{"jobtitle":"CTO","city":"Oslo"}}`
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recs[i] = v.do("dst-act-hubspot", v.bearer("hub_active"), body, func(r *http.Request) { r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1", i+1) })
		}(i)
	}
	close(start)
	wg.Wait()
	fresh, ids := 0, map[string]bool{}
	for i, rec := range recs {
		if rec.Code != 200 {
			t.Fatalf("call %d: %d %s", i, rec.Code, rec.Body)
		}
		var b struct {
			ID     string `json:"id"`
			Replay bool   `json:"replay"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		ids[b.ID] = true
		if !b.Replay {
			fresh++
		}
	}
	if fresh != 1 || len(ids) != 1 {
		t.Fatalf("fresh=%d distinct ids=%d", fresh, len(ids))
	}
	if c := destOwnerInt(t, `SELECT count(*) FROM destination_inbound_receipts WHERE external_event_id = 'evt-conc'`); c != 1 {
		t.Fatalf("%d receipts", c)
	}
}

// TestInboundReplayIndependentOfSequences: a replay returns the stored receipt whatever the sequences are (they are not
// consulted; ids are generated) and never rewrites the lead.
func TestInboundReplayIndependentOfSequences(t *testing.T) {
	v := newInbEnv(t)
	body := `{"external_event_id":"evt-seq","lead_id":5,"fields":{"city":"Bergen"}}`
	first := v.do("dst-act-hubspot", v.bearer("hub_active"), body)
	ownerExecT(t, `SELECT setval(pg_get_serial_sequence('leads','id'), 1, false)`)
	ownerExecT(t, `UPDATE leads SET city = 'Elsewhere' WHERE id = 5 AND workspace_id = 'ws-alpha'`)
	again := v.do("dst-act-hubspot", v.bearer("hub_active"), body)
	var a, b struct {
		ID     string `json:"id"`
		Replay bool   `json:"replay"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if first.Code != 200 || again.Code != 200 || a.Replay || !b.Replay || a.ID != b.ID {
		t.Fatalf("%d %s / %d %s", first.Code, first.Body, again.Code, again.Body)
	}
	var city string
	if err := wcrudOwnerQueryRow(t, `SELECT city FROM leads WHERE id = 5 AND workspace_id = 'ws-alpha'`).Scan(&city); err != nil || city != "Elsewhere" {
		t.Fatalf("replay rewrote the lead: %q %v", city, err)
	}
	ownerExecT(t, `SELECT setval(pg_get_serial_sequence('leads','id'), GREATEST(1000, (SELECT max(id) FROM leads)))`)
}

// TestInboundWorkspaceFromTokenNotHeader: the workspace is the token row's. A forged X-Workspace-Id, a workspace_id in the body
// and a foreign lead id change nothing.
func TestInboundWorkspaceFromTokenNotHeader(t *testing.T) {
	v := newInbEnv(t)
	forge := func(r *http.Request) { r.Header.Set("X-Workspace-Id", "ws-beta") }
	rec := v.do("dst-act-hubspot", v.bearer("hub_active"),
		`{"external_event_id":"evt-forge","lead_id":5,"workspace_id":"ws-beta","fields":{"city":"Forged"}}`, forge)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if v.receiptCount(t, "ws-beta") != destOwnerInt(t, `SELECT count(*) FROM destination_inbound_receipts WHERE workspace_id='ws-beta'`) ||
		destOwnerInt(t, `SELECT count(*) FROM destination_inbound_receipts WHERE external_event_id='evt-forge' AND workspace_id='ws-alpha'`) != 1 ||
		destOwnerInt(t, `SELECT count(*) FROM destination_inbound_receipts WHERE external_event_id='evt-forge' AND workspace_id='ws-beta'`) != 0 {
		t.Fatal("receipt not in the token's workspace")
	}
	// the beta token cannot be turned into an alpha caller by the header, nor an alpha token into a beta caller
	if rec := v.do("dst-act-hubspot", v.bearer("beta_active"), inbEvent("evt-forge-2"), func(r *http.Request) { r.Header.Set("X-Workspace-Id", "ws-alpha") }); rec.Code != 401 {
		t.Fatalf("beta token on alpha dest with forged header: %d", rec.Code)
	}
	if rec := v.do("dst-beta-crm", v.bearer("hub_active"), inbEvent("evt-forge-3"), forge); rec.Code != 401 {
		t.Fatalf("alpha token on beta dest with forged header: %d", rec.Code)
	}
	// a beta lead id through an alpha token is unmatched, and beta's lead is untouched
	before := destOwnerInt(t, `SELECT count(*) FROM leads WHERE id = 17 AND workspace_id = 'ws-beta' AND city = 'Forged'`)
	rec = v.do("dst-act-hubspot", v.bearer("hub_active"), `{"external_event_id":"evt-forge-4","lead_id":17,"fields":{"city":"Forged"}}`, forge)
	var b struct {
		Status string  `json:"status"`
		Error  *string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if rec.Code != 200 || b.Error == nil || *b.Error != destinbound.UnmatchedError ||
		destOwnerInt(t, `SELECT count(*) FROM leads WHERE id = 17 AND workspace_id = 'ws-beta' AND city = 'Forged'`) != before {
		t.Fatalf("foreign lead: %d %s", rec.Code, rec.Body)
	}
}

// TestInboundTokenLifecycle: rotate (old token dies at once), revoke and a deleted destination all end in the one 401.
func TestInboundTokenLifecycle(t *testing.T) {
	v := newInbEnv(t)
	ctx := context.Background()
	var fresh destinbound.RotateResult
	if err := tenancy.InWorkspace(ctx, v.pool, "ws-alpha", func(tx pgx.Tx) (err error) {
		fresh, err = destinbound.Rotate(ctx, tx, "ws-alpha", "dst-act-hubspot", 1, time.Now())
		return
	}); err != nil {
		t.Fatal(err)
	}
	if rec := v.do("dst-act-hubspot", v.bearer("hub_active"), inbEvent("evt-r-1")); rec.Code != 401 {
		t.Fatalf("old token after rotate: %d", rec.Code)
	}
	if rec := v.do("dst-act-hubspot", "Bearer "+fresh.Token, inbEvent("evt-r-2")); rec.Code != 200 {
		t.Fatalf("new token: %d %s", rec.Code, rec.Body)
	}
	if err := tenancy.InWorkspace(ctx, v.pool, "ws-alpha", func(tx pgx.Tx) error {
		_, err := destinbound.Revoke(ctx, tx, "ws-alpha", "dst-act-hubspot", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec := v.do("dst-act-hubspot", "Bearer "+fresh.Token, inbEvent("evt-r-3")); rec.Code != 401 {
		t.Fatalf("revoked: %d", rec.Code)
	}
	ownerExecT(t, `DELETE FROM destination_inbound_tokens WHERE destination_id = 'dst-act-salesforce'`)
	ownerExecT(t, `DELETE FROM audience_destinations WHERE id = 'dst-act-salesforce'`)
	if rec := v.do("dst-act-salesforce", v.bearer("sf_expired"), inbEvent("evt-d")); rec.Code != 401 {
		t.Fatalf("deleted destination: %d", rec.Code)
	}
}

// TestInboundNoTokenInLogsOrErrors: a raw token never reaches the logs or any response body.
func TestInboundNoTokenInLogsOrErrors(t *testing.T) {
	v := newInbEnv(t)
	var bodies strings.Builder
	for _, tc := range []struct{ dest, key, body string }{
		{"dst-act-hubspot", "hub_active", inbEvent("evt-log")},
		{"dst-act-hubspot", "hub_revoked", inbEvent("evt-log")},
		{"dst-act-hubspot", "hub_active", `{"external_event_id":5}`},
		{"dst-act-salesforce", "hub_active", inbEvent("evt-log")},
	} {
		bodies.WriteString(v.do(tc.dest, v.bearer(tc.key), tc.body).Body.String())
	}
	for _, raw := range v.tok {
		if strings.Contains(v.logs.String(), raw) || strings.Contains(bodies.String(), raw) {
			t.Fatalf("raw token %.12s... leaked", raw)
		}
	}
}

// TestInboundRequiresReadCommitted: reconcile's concurrent-replay handling relies on READ COMMITTED; a pool defaulting to a
// stricter level fails closed (500, nothing written) instead of running under assumptions that do not hold.
func TestInboundRequiresReadCommitted(t *testing.T) {
	v := newInbEnv(t, inbOpts{wrap: func(p *pgxpool.Pool) *pgxpool.Pool {
		cfg := p.Config().Copy()
		cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "serializable"
		strict, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(strict.Close)
		return strict
	}})
	rec := v.do("dst-act-hubspot", v.bearer("hub_active"), inbEvent("evt-ser"))
	if rec.Code != 500 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Internal Server Error"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if destOwnerInt(t, `SELECT count(*) FROM destination_inbound_receipts WHERE external_event_id = 'evt-ser'`) != 0 {
		t.Fatal("receipt written under a non READ COMMITTED transaction")
	}
}
