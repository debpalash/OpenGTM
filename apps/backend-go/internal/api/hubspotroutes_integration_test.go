//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	hsGood    = "ctk_hubspot_alpha_0001"
	hsConnect = "ctk_hubspot_connect_0002"
	hsRevoked = "ctk_hubspot_revoked_0003"
)

// fakeHubSpot is the Go port of record_vendor_transcripts.py hubspot_server (the portal the
// fixtures were recorded against): token aware, one existing contact, one create conflict.
type fakeHubSpot struct {
	*httptest.Server
	mu       sync.Mutex
	requests int
	tokens   []string
	created  int
	onSearch func(n int) // called after the n-th search request (client disconnect test)
	searches int
	echoTok  bool // 401 bodies echo the bearer token (scrubbing test)
}

func newFakeHubSpot(t *testing.T) *fakeHubSpot {
	f := &fakeHubSpot{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeHubSpot) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// hsErr writes HubSpot's error body in the vendor's key order (status, message, category).
func hsErr(w http.ResponseWriter, status int, msg, category string) {
	m, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"status":"error","message":%s,"category":%q}`, m, category)
}

func hsJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

var hsContacts = []map[string]any{
	{"id": "501", "properties": map[string]any{"email": "hello@globex.example.com", "firstname": "Gil", "lastname": "Obex", "company": "Globex", "phone": "+15550101", "jobtitle": "CTO"},
		"createdAt": "2026-08-01T10:00:00.000Z", "updatedAt": "2026-08-02T10:00:00.000Z", "archived": false},
	{"id": "502", "properties": map[string]any{"email": "ana@initech.example.com", "firstname": "Ana", "lastname": "Pérez", "company": "Initech", "phone": nil, "jobtitle": nil},
		"createdAt": "2026-08-03T10:00:00.000Z", "updatedAt": "2026-08-03T10:00:00.000Z", "archived": false},
	{"id": "503", "properties": map[string]any{"email": "zoe@umbrella.example.com", "firstname": "Zoë", "lastname": "Müller", "company": "Umbrella", "phone": "+15550103", "jobtitle": "CEO"},
		"createdAt": "2026-08-04T10:00:00.000Z", "updatedAt": "2026-08-05T10:00:00.000Z", "archived": false},
}

func (f *fakeHubSpot) serve(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests++
	f.tokens = append(f.tokens, tok)
	f.mu.Unlock()
	switch {
	case tok == hsRevoked:
		msg := "The OAuth token used to make this call expired."
		if f.echoTok {
			msg += " " + tok
		}
		hsErr(w, 401, msg, "EXPIRED_AUTHENTICATION")
	case tok != hsGood && tok != hsConnect:
		hsErr(w, 401, "Authentication credentials not found.", "INVALID_AUTHENTICATION")
	case r.Method == "GET" && r.URL.Path == "/account-info/v3/details":
		hsJSON(w, 200, map[string]any{"portalId": 4242424, "accountType": "STANDARD", "timeZone": "UTC", "currency": "USD"})
	case r.Method == "POST" && r.URL.Path == "/crm/v3/objects/contacts/search":
		var q struct {
			FilterGroups []struct {
				Filters []struct {
					Value string `json:"value"`
				} `json:"filters"`
			} `json:"filterGroups"`
		}
		_ = json.Unmarshal(body, &q)
		email := q.FilterGroups[0].Filters[0].Value
		if email == "hello@globex.example.com" {
			hsJSON(w, 200, map[string]any{"total": 1, "results": []any{map[string]any{"id": "501", "properties": map[string]any{"email": email}}}})
		} else {
			hsJSON(w, 200, map[string]any{"total": 0, "results": []any{}})
		}
		f.mu.Lock()
		f.searches++
		n, cb := f.searches, f.onSearch
		f.mu.Unlock()
		if cb != nil {
			cb(n)
		}
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/crm/v3/objects/contacts/"):
		var q struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(body, &q)
		hsJSON(w, 200, map[string]any{"id": r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "properties": q.Properties})
	case r.Method == "POST" && r.URL.Path == "/crm/v3/objects/contacts":
		var q struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(body, &q)
		if q.Properties["email"] == "info@eco.example.com" {
			hsErr(w, 409, "Contact already exists. Existing ID: 777", "CONFLICT")
			return
		}
		f.mu.Lock()
		f.created++
		id := 900 + f.created
		f.mu.Unlock()
		hsJSON(w, 201, map[string]any{"id": fmt.Sprint(id), "properties": map[string]any{"email": q.Properties["email"]}})
	case r.Method == "GET" && r.URL.Path == "/crm/v3/objects/contacts":
		var limit int
		fmt.Sscan(r.URL.Query().Get("limit"), &limit)
		if limit < 1 || limit > 100 {
			hsErr(w, 400, "limit must be between 1 and 100", "VALIDATION_ERROR")
			return
		}
		hsJSON(w, 200, map[string]any{"results": hsContacts[:min(limit, len(hsContacts))]})
	default:
		hsJSON(w, 404, map[string]any{"status": "error"})
	}
}

type hubspotEnv struct {
	leadsEnv
	fake *fakeHubSpot
	res  *secrets.Resolver
}

func newHubspotEnv(t *testing.T) hubspotEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "hubspot-routes-test-key", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(string) string { return "" }
	res := secrets.NewResolver(envl, secrets.NewPgStore(pool), getenv)
	fake := newFakeHubSpot(t)
	old := newHubSpotClient
	newHubSpotClient = func(r *secrets.Resolver) *crm.HubSpot {
		return &crm.HubSpot{Secrets: r, HTTP: fake.Client(), BaseURL: fake.URL, Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
	}
	t.Cleanup(func() { newHubSpotClient = old })
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true, GoConnectorRoutesEnabled: true},
		Envelope: envl, Getenv: getenv}
	RegisterHubspot(r, d)
	e := hubspotEnv{leadsEnv: leadsEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}, fake: fake, res: res}
	e.purge()
	e.purgeSecrets()
	e.seedLeads(t)
	// registered after the workspaces' own cleanups so they run first (LIFO)
	t.Cleanup(e.purgeSecrets)
	t.Cleanup(e.purge)
	e.seed(t, "erin", pythonHash, true, "user", false)
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('leads','id'),
		GREATEST(1000, (SELECT COALESCE(max(id), 0) FROM leads), (SELECT last_value FROM leads_id_seq)))`)
	return e
}

func (e hubspotEnv) purgeSecrets() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "DELETE FROM workspace_settings WHERE key = 'HUBSPOT_TOKEN'")
			return err
		})
	}
}

func (e hubspotEnv) setToken(t *testing.T, ws, tok string) {
	t.Helper()
	if err := e.res.Set(context.Background(), ws, "HUBSPOT_TOKEN", tok); err != nil {
		t.Fatal(err)
	}
}

func (e hubspotEnv) call(who, ws, method, path, body string) (*httptest.ResponseRecorder, contract.Resp) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	return e.do(method, path, ct, body, hdr)
}

func (e hubspotEnv) alpha(t *testing.T) { t.Helper(); e.setToken(t, "ws-alpha", hsGood) }

// hubspotActor says who sent a recorded request (the recorder redacts the credentials).
func hubspotActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case name == "status_nonmember_erin_beta":
		return "erin", "ws-beta"
	case strings.HasSuffix(name, "_beta"), strings.HasPrefix(name, "status_beta"):
		return "bob", "ws-beta"
	case strings.Contains(name, "_bob_alpha"):
		return "bob", "ws-alpha"
	}
	return "alice", "ws-alpha"
}

// Fixtures whose recorded 200 or 500 the Go port deliberately answers as 422 (D4: sync limit
// 0..1000, contacts limit <= 1000; limit 0 and negative contacts limits stay Python parity).
var hubspotLimitDeviations = map[string]string{
	"sync_limit_100000": "body", "sync_limit_negative": "body", "contacts_limit_100000": "query",
}

func (e hubspotEnv) replay(t *testing.T, name string) {
	t.Helper()
	fx := contract.Load(t, "hubspot/"+name)
	who, ws := hubspotActor(name)
	_, got := e.call(who, ws, fx.Request.Method, fx.Request.Path, fixtureBody(fx))
	if loc, dev := hubspotLimitDeviations[name]; dev {
		if got.Status != 422 || !strings.Contains(string(got.Body), `"`+loc+`"`) || !strings.Contains(string(got.Body), `"limit"`) {
			t.Errorf("%s: want 422 on %s.limit, got %d %s", name, loc, got.Status, got.Body)
		}
		return
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
}

func TestStatusMatchesFixtures(t *testing.T) {
	e := newHubspotEnv(t)
	e.alpha(t)
	for _, n := range []string{"status_alpha_configured", "status_viewer_bob_alpha", "status_beta_not_configured",
		"status_beta_still_not_configured", "status_no_auth", "status_nonmember_erin_beta"} {
		e.replay(t, n)
	}
	// the revoked token answers the recorded 401 as a connected:false body, then recovers
	e.replay(t, "connect_editor_revoked")
	e.replay(t, "status_alpha_upstream_401")
	e.replay(t, "connect_editor_restore")
	e.replay(t, "status_alpha_after_restore")
}

func TestConnectStoresPerWorkspaceSecret(t *testing.T) {
	e := newHubspotEnv(t)
	rec, _ := e.call("bob", "ws-beta", "POST", "/api/crm/hubspot/connect", `{"token":"beta-secret-token-1"}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "beta-secret-token-1") {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	if v, _, err := e.res.Get(ctx, "ws-beta", "HUBSPOT_TOKEN"); err != nil || v != "beta-secret-token-1" {
		t.Fatalf("beta secret = %q, %v", v, err)
	}
	if v, _, _ := e.res.Get(ctx, "ws-alpha", "HUBSPOT_TOKEN"); v != "" {
		t.Fatalf("alpha must not see beta's token: %q", v)
	}
	// stored encrypted, never plaintext
	var raw string
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-beta", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT value FROM workspace_settings WHERE workspace_id='ws-beta' AND key='HUBSPOT_TOKEN'`).Scan(&raw)
	}); err != nil || raw == "" || strings.Contains(raw, "beta-secret-token-1") {
		t.Fatalf("stored value %q, %v", raw, err)
	}
}

func TestConnectViewerForbidden(t *testing.T) {
	e := newHubspotEnv(t)
	e.replay(t, "connect_viewer_bob_alpha")
	e.replay(t, "connect_no_auth")
	if v, _, _ := e.res.Get(context.Background(), "ws-alpha", "HUBSPOT_TOKEN"); v != "" {
		t.Fatalf("a forbidden connect stored %q", v)
	}
}

func TestConnectEmptyTokenValidation(t *testing.T) {
	e := newHubspotEnv(t)
	e.alpha(t)
	for _, n := range []string{"connect_missing_token", "connect_token_not_string", "connect_empty_token"} {
		e.replay(t, n)
	}
	// blank token: 200 as Python, and the saved secret is kept
	if v, _, _ := e.res.Get(context.Background(), "ws-alpha", "HUBSPOT_TOKEN"); v != hsGood {
		t.Fatalf("blank connect clobbered the secret: %q", v)
	}
}

func TestSyncMatchesFixtures(t *testing.T) {
	e := newHubspotEnv(t)
	e.alpha(t)
	for _, n := range []string{"sync_default", "sync_lead_ids", "sync_lead_ids_foreign_and_unknown", "sync_limit_3", "sync_tier_hot",
		"sync_tier_unknown", "sync_no_email_lead", "sync_only_foreign_ids", "sync_bad_body", "sync_no_auth", "sync_viewer_bob_alpha",
		"sync_not_connected_beta", "sync_limit_0", "sync_limit_100000", "sync_limit_negative"} {
		e.replay(t, n)
	}
	e.replay(t, "connect_editor_revoked")
	e.replay(t, "sync_revoked_token_all_fail")
}

func TestSyncForeignLeadIDsDropped(t *testing.T) {
	e := newHubspotEnv(t)
	e.alpha(t)
	before := e.fake.count()
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/crm/hubspot/sync", `{"lead_ids":[17,18,99999]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "No leads with emails to sync") || e.fake.count() != before {
		t.Fatalf("foreign ids: %d %s (requests %d->%d)", rec.Code, rec.Body.String(), before, e.fake.count())
	}
}

func TestSyncNoEmailLeadsMessage(t *testing.T) {
	e := newHubspotEnv(t)
	e.alpha(t)
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/crm/hubspot/sync", `{"lead_ids":[7,9]}`)
	if rec.Body.String() != `{"created":0,"updated":0,"failed":0,"message":"No leads with emails to sync"}`+"\n" &&
		strings.TrimSpace(rec.Body.String()) != `{"created":0,"updated":0,"failed":0,"message":"No leads with emails to sync"}` {
		t.Fatalf("body %s", rec.Body.String())
	}
}

func TestSyncNotConnected400(t *testing.T) {
	e := newHubspotEnv(t)
	rec, _ := e.call("bob", "ws-beta", "POST", "/api/crm/hubspot/sync", `{}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "HubSpot not connected. Add token first.") || e.fake.count() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSyncLimitBounds422(t *testing.T) {
	e := newHubspotEnv(t)
	e.alpha(t)
	for _, c := range []struct{ body, typ string }{
		{`{"limit":-1}`, "greater_than_equal"}, {`{"limit":1001}`, "less_than_equal"},
	} {
		rec, _ := e.call("alice", "ws-alpha", "POST", "/api/crm/hubspot/sync", c.body)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), c.typ) || !strings.Contains(rec.Body.String(), `"limit"`) {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body.String())
		}
	}
	ids := make([]string, 1001)
	for i := range ids {
		ids[i] = "1"
	}
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/crm/hubspot/sync", `{"lead_ids":[`+strings.Join(ids, ",")+`]}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"ctx":{"field_type":"List","max_length":1000,"actual_length":1001}`) {
		t.Errorf("1001 lead_ids: %d %.200s", rec.Code, rec.Body.String())
	}
	// limit 0 is Python's 200 "No leads" answer
	if rec, _ := e.call("alice", "ws-alpha", "POST", "/api/crm/hubspot/sync", `{"limit":0}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), "No leads with emails to sync") {
		t.Errorf("limit 0: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := e.call("alice", "ws-alpha", "POST", "/api/crm/hubspot/sync", `{"limit":1001}`); !strings.Contains(rec.Body.String(), "Input should be less than or equal to 1000") {
		t.Errorf("limit 1001: %s", rec.Body.String())
	}
	for _, q := range []string{"1001", "100000"} {
		rec, _ := e.call("alice", "ws-alpha", "GET", "/api/crm/hubspot/contacts?limit="+q, "")
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"query"`) {
			t.Errorf("contacts limit=%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	// contacts below 1 reach HubSpot (which answers 400 -> empty list), as Python
	for _, q := range []string{"0", "-1"} {
		if rec, _ := e.call("alice", "ws-alpha", "GET", "/api/crm/hubspot/contacts?limit="+q, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":0`) {
			t.Errorf("contacts limit=%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	rec, _ = e.call("alice", "ws-alpha", "POST", "/api/crm/hubspot/sync", `{"limit":1000}`)
	if rec.Code != 200 {
		t.Errorf("limit 1000: %d", rec.Code)
	}
}

func TestSyncErrorsCappedAtFive(t *testing.T) {
	e := newHubspotEnv(t)
	e.setToken(t, "ws-alpha", hsRevoked)
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/crm/hubspot/sync", `{}`)
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	errs, _ := raw["errors"].([]any)
	if rec.Code != 200 || len(errs) != 5 || raw["failed"].(float64) != 10 || raw["total_synced"].(float64) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSyncTokenNeverInResponse(t *testing.T) {
	e := newHubspotEnv(t)
	e.fake.echoTok = true
	e.setToken(t, "ws-alpha", hsRevoked)
	for _, c := range [][3]string{
		{"POST", "/api/crm/hubspot/sync", `{"lead_ids":[1,2]}`},
		{"GET", "/api/crm/hubspot/status", ""},
		{"GET", "/api/crm/hubspot/contacts", ""},
		{"POST", "/api/crm/hubspot/connect", `{"token":"` + hsRevoked + `"}`},
	} {
		rec, _ := e.call("alice", "ws-alpha", c[0], c[1], c[2])
		if strings.Contains(rec.Body.String(), hsRevoked) {
			t.Errorf("%s %s echoes the token: %s", c[0], c[1], rec.Body.String())
		}
	}
}

func TestContactsMatchesFixture(t *testing.T) {
	e := newHubspotEnv(t)
	e.alpha(t)
	for _, n := range []string{"contacts_ok", "contacts_limit_1", "contacts_viewer_bob_alpha", "contacts_no_auth",
		"contacts_limit_not_int", "contacts_limit_0", "contacts_limit_100000", "contacts_limit_negative"} {
		e.replay(t, n)
	}
}

func TestContactsNotConnected400(t *testing.T) {
	e := newHubspotEnv(t)
	e.replay(t, "contacts_not_connected_beta")
	if e.fake.count() != 0 {
		t.Fatal("unconnected contacts called HubSpot")
	}
}

func TestOtherWorkspaceTokenNotUsed(t *testing.T) {
	e := newHubspotEnv(t)
	e.alpha(t)
	for _, c := range [][3]string{{"POST", "/api/crm/hubspot/sync", `{}`}, {"GET", "/api/crm/hubspot/contacts", ""}} {
		rec, _ := e.call("bob", "ws-beta", c[0], c[1], c[2])
		if rec.Code != 400 {
			t.Errorf("%s as beta with only alpha configured: %d %s", c[1], rec.Code, rec.Body.String())
		}
	}
	rec, _ := e.call("bob", "ws-beta", "GET", "/api/crm/hubspot/status", "")
	if !strings.Contains(rec.Body.String(), "No HubSpot token configured") || e.fake.count() != 0 {
		t.Fatalf("beta status %s, upstream requests %d", rec.Body.String(), e.fake.count())
	}
	e.setToken(t, "ws-beta", hsConnect)
	e.call("bob", "ws-beta", "GET", "/api/crm/hubspot/status", "")
	e.call("alice", "ws-alpha", "GET", "/api/crm/hubspot/status", "")
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	if strings.Join(e.fake.tokens, ",") != hsConnect+","+hsGood {
		t.Fatalf("tokens used %v", e.fake.tokens)
	}
}

func TestSyncStopsOnClientDisconnect(t *testing.T) {
	e := newHubspotEnv(t)
	e.alpha(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.fake.onSearch = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	req := httptest.NewRequest("POST", "/api/crm/hubspot/sync", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Workspace-Id", "ws-alpha")
	for k, v := range bearer("alice") {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { e.h.ServeHTTP(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sync did not stop after the client went away")
	}
	e.fake.mu.Lock()
	n := e.fake.searches
	e.fake.mu.Unlock()
	if n > 3 {
		t.Fatalf("%d leads pushed after the disconnect (10 were queued)", n)
	}
}
