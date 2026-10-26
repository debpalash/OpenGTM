//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Plan 14 Task 18: the lead utility routes (score, verify-email, tech-stack) replayed against the recorded
// Python fixtures (testdata/contract/leadutils). The seed mirrors contract_seed.py: leads 1 and 14 and the
// leadgen leads 101 (alpha) and 106 (beta). The recorder replaced the SMTP verifier by a scripted one that
// answers by local-part prefix (leadgen_contract_server.py); scriptedVerifier is its Go twin.

type utilsEnv struct {
	mcpEnv
	verifyCalls atomic.Int64
	mu          sync.Mutex
	verifyWS    []string // workspace of every Reacher resolution
}

// scriptedVerifier is _ScriptedVerifier of the recorder.
type scriptedVerifier struct{ calls *atomic.Int64 }

func (scriptedVerifier) Name() string                            { return "smtp_probe" }
func (scriptedVerifier) Available(context.Context) (bool, error) { return true, nil }
func (s scriptedVerifier) Verify(_ context.Context, email string) (emailverify.Result, error) {
	s.calls.Add(1)
	local := strings.ToLower(strings.SplitN(email, "@", 2)[0])
	for _, c := range []struct {
		prefix, status, detail string
		conf                   float64
	}{
		{"valid", emailverify.StatusValid, "250 recipient ok", 0.95},
		{"invalid", emailverify.StatusInvalid, "550 mailbox unavailable", 0.9},
		{"catchall", emailverify.StatusCatchAll, "accepts any recipient", 0.5},
	} {
		if strings.HasPrefix(local, c.prefix) {
			return emailverify.Result{Email: email, Status: c.status, Source: "smtp_probe", Confidence: c.conf, Detail: c.detail, Attempts: []emailverify.Attempt{}}, nil
		}
	}
	return emailverify.Result{Email: email, Status: emailverify.StatusUnknown, Source: "smtp_probe", Detail: "smtp timeout", Attempts: []emailverify.Attempt{}}, nil
}

// wsReacher is a Reacher that records the workspace it was resolved for and answers definitively.
type wsReacher struct {
	e  *utilsEnv
	ws string
}

func (wsReacher) Name() string                            { return "reacher" }
func (wsReacher) Available(context.Context) (bool, error) { return true, nil }
func (r wsReacher) Verify(_ context.Context, email string) (emailverify.Result, error) {
	return emailverify.Result{Email: email, Status: emailverify.StatusValid, Source: "reacher", Confidence: 0.95, Detail: r.ws, Attempts: []emailverify.Attempt{}}, nil
}

func newUtilsEnv(t *testing.T, mut ...func(*Deps)) *utilsEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	e := &utilsEnv{}
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Limiter: ratelimit.New(nil), CollectionLedgerBackend: "postgres",
		Getenv: func(string) string { return "" },
		Flags:  config.Flags{PGLeadStore: true, Leadgen: config.LeadgenSettings{GoCollectionRoutesEnabled: true}}}
	for _, m := range mut {
		m(&d)
	}
	leadUtilsVerifyDeps = func(Deps) emailverify.Deps {
		dd := emailverify.NewDeps()
		dd.Registry = emailverify.NewRegistry()
		dd.Registry.Register(scriptedVerifier{calls: &e.verifyCalls}, false)
		return dd
	}
	RegisterLeadUtils(r, d)
	e.mcpEnv = mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}
	t.Cleanup(func() {
		leadUtilsVerifyDeps = nil
		e.purge(t)
	})
	e.purge(t)
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member") // the viewer
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	e.seedLeads(t)
	return e
}

func (e *utilsEnv) purge(t *testing.T) {
	t.Helper()
	ownerExec(t, `DELETE FROM leads WHERE workspace_id IN ('ws-alpha','ws-beta')`)
}

func (e *utilsEnv) seedLeads(t *testing.T) {
	t.Helper()
	type l struct {
		id                                                                       int
		ws, company, city, site, mail, phone, person, spec, size, desc, linkedin string
	}
	for _, r := range []l{
		{1, "ws-alpha", "Acme 01", "Austin", "https://acme01.example.com", "info@acme01.example.com", "", "", "", "", "", ""},
		{5, "ws-alpha", "Nordlicht GmbH", "", "https://nordlicht.example.com", "kontakt@nordlicht.example.com", "", "", "Lighting", "51-200", "Empty city is a real key", ""},
		{8, "ws-alpha", "Zenith Robotics", "Mumbai", "", "sales@zenithrobotics.example.com", "+91-22-5550-1001", "", "Robotics", "N/A", "nan", ""},
		{14, "ws-alpha", "Hot Pursuit Labs", "Austin", "https://hotpursuit.example.com", "ceo@hotpursuit.example.com", "+1 512 555 0014", "Sam Lee", "Analytics", "51-200", "Our best account", "https://linkedin.com/company/hotpursuit"},
		{101, "ws-alpha", "Mumbai Talent Partners", "Mumbai", "https://mtp.example.com", "hr@mtp.example.com", "+91 22 5550 2001", "", "", "", "", ""},
		{106, "ws-beta", "Beta Staffing Co", "Chennai", "https://betastaff.example.com", "", "", "", "", "", "", ""},
	} {
		ownerExec(t, `INSERT INTO leads (id, workspace_id, company, city, website, email, phone, contact_person, specialization, company_size, description, linkedin_url, status, source, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'new','contract','2026-08-29T10:00:00','2026-08-29T10:00:00')`,
			r.id, r.ws, r.company, r.city, r.site, r.mail, r.phone, r.person, r.spec, r.size, r.desc, r.linkedin)
	}
}

func (e *utilsEnv) post(who, ws, path, body string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do("POST", path, "application/json", body, hdr)
	return rec
}

// utilsActor says who sent a recorded request (the recorder redacts Authorization and X-Workspace-Id).
func utilsActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case name == "score_beta_lead_id":
		return "bob", "ws-beta"
	case strings.HasSuffix(name, "_bob_ok"), name == "verify_email_viewer_ok", name == "tech_stack_viewer_409":
		return "bob", "ws-alpha"
	}
	return "alice", ""
}

func utilsFixtureNames(t *testing.T) []string {
	var out []string
	for _, full := range contractFixtureNames(t, "leadutils") {
		n := strings.TrimPrefix(full, "leadutils/")
		if strings.HasPrefix(n, "domain_intel_") {
			continue // Task 19
		}
		out = append(out, n)
	}
	return out
}

func TestLeadUtilsMatchFixtures(t *testing.T) {
	e := newUtilsEnv(t)
	n := 0
	for _, name := range utilsFixtureNames(t) {
		fx := contract.Load(t, "leadutils/"+name)
		if name == "score_inline_email_int_500" {
			continue // Python bug: TypeError -> bare 500. Go answers 422 (TestScoreInlineWrongTypes422)
		}
		who, ws := utilsActor(name)
		rec := e.post(who, ws, fx.Request.Path, string(fx.Request.Body))
		_, got := e.doResp(rec)
		contract.AssertEqual(t, got, fx.Response)
		if t.Failed() {
			t.Fatalf("fixture %s: %s", name, rec.Body.String())
		}
		n++
	}
	if n < 50 {
		t.Fatalf("only %d fixtures replayed", n)
	}
}

func (e *utilsEnv) doResp(rec *httptest.ResponseRecorder) (*httptest.ResponseRecorder, contract.Resp) {
	h := map[string]string{}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			h[k] = v
		}
	}
	return rec, contract.Resp{Status: rec.Code, Headers: h, Body: json.RawMessage(rec.Body.Bytes())}
}

func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	return m
}

func TestScoreByIDVsInline(t *testing.T) {
	e := newUtilsEnv(t)
	byID := jsonBody(t, e.post("alice", "", "/api/leads/score", `{"lead_id":14}`))
	inline := jsonBody(t, e.post("alice", "", "/api/leads/score",
		`{"lead":{"company":"Hot Pursuit Labs","city":"Austin","website":"https://hotpursuit.example.com","email":"ceo@hotpursuit.example.com","phone":"+1 512 555 0014","contact_person":"Sam Lee","specialization":"Analytics","company_size":"51-200","linkedin_url":"https://linkedin.com/company/hotpursuit"}}`))
	if byID["score"] != inline["score"] || byID["tier"] != inline["tier"] || byID["lead_id"] != float64(14) || inline["lead_id"] != nil {
		t.Fatalf("by id %v, inline %v", byID, inline)
	}
	// lead_id wins over lead; the stored lead is scored, never the inline one
	both := jsonBody(t, e.post("alice", "", "/api/leads/score", `{"lead_id":14,"lead":{"company":"Ignored"}}`))
	if both["company"] != "Hot Pursuit Labs" {
		t.Fatalf("%v", both)
	}
	// nothing is stored by an inline score
	if n := ownerCount(t, `SELECT count(*) FROM leads WHERE workspace_id='ws-alpha' AND company='Ignored'`); n != 0 {
		t.Fatalf("%d rows", n)
	}
	rec := e.post("alice", "", "/api/leads/score", `{"lead_id":14}`)
	if got := rec.Body.String(); got != `{"lead_id":14,"company":"Hot Pursuit Labs","score":70,"tier":"warm"}` {
		t.Fatalf("key order / bytes: %s", got)
	}
}

func TestScoreNeitherIs400(t *testing.T) {
	e := newUtilsEnv(t)
	for _, b := range []string{`{}`, `{"lead":{}}`, `{"lead_id":null,"lead":null}`} {
		rec := e.post("alice", "", "/api/leads/score", b)
		if rec.Code != 400 || rec.Body.String() != `{"detail":"provide lead_id or lead"}` {
			t.Errorf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
	}
}

func TestScoreMissingLead404(t *testing.T) {
	e := newUtilsEnv(t)
	// 106 belongs to ws-beta; 99999999999 is outside int4; none may leak or wrap around to another row
	for _, b := range []string{`{"lead_id":99999}`, `{"lead_id":0}`, `{"lead_id":-1}`, `{"lead_id":106}`, `{"lead_id":4294967297}`, `{"lead_id":99999999999999999999}`} {
		rec := e.post("alice", "", "/api/leads/score", b)
		if rec.Code != 404 || rec.Body.String() != `{"detail":"Lead not found"}` {
			t.Errorf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
	}
	// the foreign lead is visible to its own workspace
	if rec := e.post("bob", "ws-beta", "/api/leads/score", `{"lead_id":106}`); rec.Code != 200 {
		t.Errorf("beta: %d %s", rec.Code, rec.Body.String())
	}
}

func TestScoreInlineWrongTypes422(t *testing.T) {
	e := newUtilsEnv(t)
	for _, b := range []string{`{"lead":{"company":"T","email":5}}`, `{"lead":{"company":"T","specialization":["a"]}}`, `{"lead":{"company":"T","email":true}}`} {
		rec := e.post("alice", "", "/api/leads/score", b)
		m := jsonBody(t, rec)
		if rec.Code != 422 || m["detail"] == nil {
			t.Errorf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "Traceback") || strings.Contains(rec.Body.String(), "goroutine") {
			t.Errorf("internals leaked: %s", rec.Body.String())
		}
	}
	// values Python scores without raising stay 200
	for _, b := range []string{`{"lead":{"company":"T","website":5}}`, `{"lead":{"company":"T","employee_count_exact":"many"}}`} {
		if rec := e.post("alice", "", "/api/leads/score", b); rec.Code != 200 {
			t.Errorf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
	}
}

func TestVerifyEmailValidation400(t *testing.T) {
	e := newUtilsEnv(t)
	for _, em := range []string{"", "   ", "not-an-email", "a b@example.com", "a@b c", "\\u00a0\\u00a0"} {
		rec := e.post("alice", "", "/api/leads/verify-email", fmt.Sprintf(`{"email":"%s"}`, em))
		if rec.Code != 400 || rec.Body.String() != `{"detail":"email must be a single address"}` {
			t.Errorf("%q: %d %s", em, rec.Code, rec.Body.String())
		}
	}
	if n := e.verifyCalls.Load(); n != 0 {
		t.Errorf("verifier ran %d times for invalid input", n)
	}
	// strip() removes Unicode whitespace too; the stripped address is echoed
	rec := e.post("alice", "", "/api/leads/verify-email", `{"email":" valid@example.com "}`)
	if m := jsonBody(t, rec); rec.Code != 200 || m["email"] != "valid@example.com" {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	rec = e.post("alice", "", "/api/leads/verify-email", `{"email":"catchall@example.com"}`)
	if got := rec.Body.String(); got != `{"email":"catchall@example.com","status":"catch_all","valid":false,"deliverable":true,"confidence":0.5,"source":"smtp_probe","detail":"accepts any recipient"}` {
		t.Errorf("bytes / key order: %s", got)
	}
	rec = e.post("alice", "", "/api/leads/verify-email", `{"email":"someone@example.com"}`)
	if !strings.Contains(rec.Body.String(), `"confidence":0.0,`) {
		t.Errorf("a float stays a float (Python 0.0): %s", rec.Body.String())
	}
}

func TestVerifyEmailCascadeWorkspaceScoped(t *testing.T) {
	e := newUtilsEnv(t)
	leadUtilsVerifyDeps = func(Deps) emailverify.Deps {
		dd := emailverify.NewDeps()
		dd.Registry = emailverify.NewRegistry()
		dd.Reacher = func(_ context.Context, ws string) emailverify.Verifier {
			e.mu.Lock()
			e.verifyWS = append(e.verifyWS, ws)
			e.mu.Unlock()
			return wsReacher{e: e, ws: ws}
		}
		return dd
	}
	// the workspace is the authenticated one, whatever the body says
	r1 := jsonBody(t, e.post("alice", "ws-alpha", "/api/leads/verify-email", `{"email":"x@example.com","workspace_id":"ws-beta"}`))
	r2 := jsonBody(t, e.post("bob", "ws-beta", "/api/leads/verify-email", `{"email":"x@example.com"}`))
	if r1["detail"] != "ws-alpha" || r2["detail"] != "ws-beta" {
		t.Fatalf("cascade ran for the wrong workspace: %v / %v", r1, r2)
	}
	if r1["source"] != "reacher" {
		t.Fatalf("%v", r1)
	}
}

func TestVerifyEmailRateLimit(t *testing.T) {
	e := newUtilsEnv(t)
	// a 422 never reaches the limited body in slowapi: it does not count; a 400 does
	for _, b := range []string{`{}`, `{"email":5}`} {
		if rec := e.post("bob", "ws-beta", "/api/leads/verify-email", b); rec.Code != 422 {
			t.Fatalf("%s: %d", b, rec.Code)
		}
	}
	for i := 1; i <= 60; i++ {
		b := `{"email":"valid@example.com"}`
		want := 200
		if i%2 == 0 {
			b, want = `{"email":"nope"}`, 400
		}
		if rec := e.post("bob", "ws-beta", "/api/leads/verify-email", b); rec.Code != want {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := e.post("bob", "ws-beta", "/api/leads/verify-email", `{"email":"valid@example.com"}`)
	if rec.Code != 429 || rec.Body.String() != `{"error":"Rate limit exceeded: 60 per 1 minute"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.post("alice", "ws-alpha", "/api/leads/verify-email", `{"email":"valid@example.com"}`); rec.Code != 200 {
		t.Fatalf("another workspace's bucket must be untouched: %d", rec.Code)
	}
}

func TestTechStackDisabled409(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	e := newUtilsEnv(t)
	for _, b := range []string{`{"domain":"example.com"}`, `{"domain":"https://example.com/path"}`, fmt.Sprintf(`{"domain":%q}`, srv.URL)} {
		rec := e.post("alice", "", "/api/leads/tech-stack", b)
		if rec.Code != 409 || rec.Body.String() != `{"detail":"website fetches are disabled; set TECH_STACK_WEBSITE_FETCH_ENABLED=1"}` {
			t.Errorf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("disabled: %d outbound requests", hits.Load())
	}
	for _, b := range []string{``, `{"domain":""}`, `{"domain":"   "}`} {
		want := 400
		if b == "" {
			want = 422
		}
		if rec := e.post("alice", "", "/api/leads/tech-stack", b); rec.Code != want {
			t.Errorf("%q: %d %s", b, rec.Code, rec.Body.String())
		}
	}
}

func enableTechFetch(d *Deps) {
	d.Getenv = func(k string) string {
		switch k {
		case "TECH_STACK_WEBSITE_FETCH_ENABLED":
			return "1"
		case "TECH_STACK_RESPECT_ROBOTS":
			return "0"
		}
		return ""
	}
}

func TestTechStackRefusesPrivateHost(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	e := newUtilsEnv(t, enableTechFetch, func(d *Deps) { d.Egress = egress.NewClient(nil) }) // production guard, no allow list
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, dom := range []string{host, "http://" + host, "localhost", "127.0.0.1", "169.254.169.254", "http://169.254.169.254/latest/meta-data/", "[::1]", "10.0.0.1", "0x7f000001", "2130706433"} {
		rec := e.post("alice", "", "/api/leads/tech-stack", fmt.Sprintf(`{"domain":%q}`, dom))
		m := jsonBody(t, rec)
		errStr, _ := m["error"].(string)
		if rec.Code != 200 || m["success"] != false || errStr == "" || m["technologies"] != nil {
			t.Errorf("%s: %d %s", dom, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "dial tcp") {
			t.Errorf("%s: raw dial error leaked: %s", dom, rec.Body.String())
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the guard let %d requests through to a loopback server", hits.Load())
	}
}

func TestTechStackRateLimit(t *testing.T) {
	e := newUtilsEnv(t)
	for _, b := range []string{`{}`, `{"domain":5}`} {
		if rec := e.post("bob", "ws-beta", "/api/leads/tech-stack", b); rec.Code != 422 {
			t.Fatalf("%s: %d", b, rec.Code)
		}
	}
	for i := 1; i <= 30; i++ {
		if rec := e.post("bob", "ws-beta", "/api/leads/tech-stack", `{"domain":"example.com"}`); rec.Code != 409 {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := e.post("bob", "ws-beta", "/api/leads/tech-stack", `{"domain":"example.com"}`)
	if rec.Code != 429 || rec.Body.String() != `{"error":"Rate limit exceeded: 30 per 1 minute"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestUtilsRequireWorkspaceMember(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	e := newUtilsEnv(t, enableTechFetch, func(d *Deps) {
		d.Config.AppEnv = "test"
		d.EgressAllowHosts = []string{"127.0.0.1"}
	})
	for path, body := range map[string]string{
		"/api/leads/score":        `{"lead_id":106}`,
		"/api/leads/verify-email": `{"email":"valid@example.com"}`,
		"/api/leads/tech-stack":   fmt.Sprintf(`{"domain":%q}`, srv.URL),
	} {
		rec := e.post("erin", "ws-beta", path, body) // erin is not a member of ws-beta
		if rec.Code != 403 {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		if rec := e.post("", "", path, body); rec.Code != 401 {
			t.Errorf("%s anonymous: %d", path, rec.Code)
		}
	}
	if e.verifyCalls.Load() != 0 || hits.Load() != 0 {
		t.Fatalf("outbound work for a non-member: verify %d, fetch %d", e.verifyCalls.Load(), hits.Load())
	}
}

// fakeTech is a tech_stack provider with a fixed result (or a blocking one).
type fakeTech struct {
	out   providers.Output
	block chan struct{}
	in    chan struct{} // receives once per call that entered
}

func (fakeTech) Name() string { return "tech_stack" }
func (f fakeTech) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), _ providers.Input) (providers.Output, error) {
	if f.in != nil {
		f.in <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return providers.Output{}, ctx.Err()
		}
	}
	return f.out, nil
}

func useFakeTech(t *testing.T, f fakeTech) {
	leadUtilsTechProvider = func() providers.Provider { return f }
	t.Cleanup(func() { leadUtilsTechProvider, leadUtilsTechTimeout = nil, 0 })
}

// Success shape: tech_stack_endpoint returns {"domain","success","technologies","fields","error"}; technologies is
// fields.get("technologies"), fields is result.fields (insertion order technologies, technographics), error None.
func TestTechStackSuccessShape(t *testing.T) {
	f := providers.NewObj()
	f.Set("technologies", "WordPress, Express")
	f.Set("technographics", `[{"name":"WordPress","category":"CMS","source":"website","confidence":0.9}]`)
	useFakeTech(t, fakeTech{out: providers.Output{Extra: f}})
	e := newUtilsEnv(t)
	rec := e.post("alice", "", "/api/leads/tech-stack", `{"domain":"  acme.com "}`)
	want := `{"domain":"acme.com","success":true,"technologies":"WordPress, Express","fields":{"technologies":"WordPress, Express","technographics":"[{\"name\":\"WordPress\",\"category\":\"CMS\",\"source\":\"website\",\"confidence\":0.9}]"},"error":null}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// failure: success false, technologies null, fields {}, error carried (not a 409 unless disabled)
	useFakeTech(t, fakeTech{out: providers.Output{Error: "No technologies detected"}})
	rec = e.post("alice", "", "/api/leads/tech-stack", `{"domain":"acme.com"}`)
	if rec.Code != 200 || rec.Body.String() != `{"domain":"acme.com","success":false,"technologies":null,"fields":{},"error":"No technologies detected"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestTechStackConcurrencyCap503(t *testing.T) {
	block := make(chan struct{})
	in := make(chan struct{}, techStackMaxInFlight+1)
	useFakeTech(t, fakeTech{block: block, in: in, out: providers.Output{Error: "No technologies detected"}})
	e := newUtilsEnv(t, func(d *Deps) { d.Limiter = nil }) // the rate limit is not under test
	var wg sync.WaitGroup
	for i := 0; i < techStackMaxInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.post("alice", "", "/api/leads/tech-stack", `{"domain":"a.com"}`)
		}()
	}
	for i := 0; i < techStackMaxInFlight; i++ {
		<-in
	}
	rec := e.post("alice", "", "/api/leads/tech-stack", `{"domain":"a.com"}`)
	if rec.Code != 503 {
		t.Errorf("over the cap: %d %s", rec.Code, rec.Body.String())
	}
	close(block)
	wg.Wait()
	if rec := e.post("alice", "", "/api/leads/tech-stack", `{"domain":"a.com"}`); rec.Code != 200 {
		t.Errorf("slots must be released: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTechStackTimeout504(t *testing.T) {
	useFakeTech(t, fakeTech{block: make(chan struct{})})
	leadUtilsTechTimeout = 50 * time.Millisecond
	e := newUtilsEnv(t)
	rec := e.post("alice", "", "/api/leads/tech-stack", `{"domain":"slow.example"}`)
	if rec.Code != 504 || rec.Body.String() != `{"detail":"Website fetch timed out"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(techStackSlots) != 0 {
		t.Fatalf("slot leaked: %d", len(techStackSlots))
	}
}
