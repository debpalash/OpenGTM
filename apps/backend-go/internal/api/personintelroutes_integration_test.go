//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/personintel"
)

type personEnv struct {
	leadsEnv
	logs *chatSyncBuf
}

// emptyWeb answers every outbound request with an empty 200 page, so the pipeline runs offline.
type emptyWeb struct{}

func (emptyWeb) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("<html></html>")), Request: r}, nil
}

func newPersonEnv(t *testing.T, hook func(*personRoutes)) *personEnv {
	t.Helper()
	le := newLeadsEnv(t)
	logs := &chatSyncBuf{}
	d := Deps{Pool: le.pool, Signer: testSigner, Identity: le.st,
		Flags:  config.Flags{PGLeadStore: true, Copilot: config.CopilotSettings{GoCopilotEnabled: true}},
		Egress: &http.Client{Transport: emptyWeb{}},
		Log:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	p := newPersonRoutes(d)
	if hook != nil {
		hook(p)
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(le.pool, nil))
	p.register(r)
	le.h = r.Handler()
	e := &personEnv{leadsEnv: le, logs: logs}
	e.truncate(t)
	t.Cleanup(func() { wcrudOwnerExecQuiet(`TRUNCATE person_intel RESTART IDENTITY`) })
	return e
}

func (e *personEnv) truncate(t *testing.T) { ownerExec(t, `TRUNCATE person_intel RESTART IDENTITY`) }

type personSeedRow struct {
	url, user, name, headline, loc, summary string
	emails, companies, edu, skills          any
	social, articles, mentions, raw         any
	status                                  string
}

func personJSON(v any, def string) string {
	if v == nil {
		return def
	}
	if raw, ok := v.(json.RawMessage); ok {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (e *personEnv) insert(t *testing.T, r personSeedRow) {
	t.Helper()
	ownerExec(t, `INSERT INTO person_intel (linkedin_url, username, name, headline, location, summary, emails, social_links,
		articles, mentions, companies, education, skills, raw_sources, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::json,$8::json,$9::json,$10::json,$11::json,$12::json,$13::json,$14::json,$15,
		'2026-08-28 12:00:00','2026-08-28 12:00:00')`,
		r.url, r.user, r.name, r.headline, r.loc, r.summary, personJSON(r.emails, "[]"), personJSON(r.social, "{}"),
		personJSON(r.articles, "[]"), personJSON(r.mentions, "[]"), personJSON(r.companies, "[]"), personJSON(r.edu, "[]"),
		personJSON(r.skills, "[]"), personJSON(r.raw, "[]"), r.status)
}

// seedContract is the three person_intel rows of contract_seed.py (_CK_PERSON_INTEL), ids 1-3.
func (e *personEnv) seedContract(t *testing.T) {
	t.Helper()
	e.truncate(t)
	e.insert(t, personSeedRow{url: "https://www.linkedin.com/in/jane-partner", user: "jane-partner", name: "Jane Partner",
		headline: "VP Partnerships at Stripe", loc: "San Francisco, CA", summary: "Leads strategic partnerships.",
		emails: []string{"jane.partner@stripe.example"}, social: map[string]string{"github": "https://github.com/jane-partner"},
		articles:  []map[string]string{{"title": "Partnering at scale", "url": "https://example.com/a"}},
		mentions:  []map[string]string{{"title": "Podcast", "url": "https://example.com/m"}},
		companies: []string{"Stripe"}, edu: []string{"Stanford University"}, skills: []string{"Partnerships", "Payments"},
		raw: json.RawMessage(`[{"source":"ddg","query":"jane-partner"}]`), status: "completed"})
	e.insert(t, personSeedRow{url: "https://www.linkedin.com/in/raj-risky", user: "raj-risky", name: "Raj Risky", status: "partial"})
	e.insert(t, personSeedRow{url: "https://www.linkedin.com/in/émilie-d", user: "émilie-d", name: "Émilie D.",
		headline: "Directrice des partenariats", loc: "Paris", summary: "Résumé — café.",
		emails:    []string{"emilie@example.fr", "e.d@example.fr"},
		social:    map[string]string{"twitter": "https://twitter.com/emilied", "github": "https://github.com/emilied"},
		companies: []string{"Acme SAS"}, edu: []string{"HEC Paris"}, skills: []string{"Alliances"}, status: "completed"})
}

func (e *personEnv) call(who, method, path, body string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	rec, _ := e.do(method, path, "application/json", body, hdr)
	return rec
}

func personRows(t *testing.T) int {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM person_intel`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPersonRoutesMatchFixtures(t *testing.T) {
	e := newPersonEnv(t, nil)
	replayed := 0
	for _, full := range contractFixtureNames(t, "person") {
		name := strings.TrimPrefix(full, "person/")
		fx := contract.Load(t, full)
		e.seedContract(t)
		if strings.Contains(name, "twice") || strings.Contains(name, "after_delete") {
			ownerExec(t, `DELETE FROM person_intel WHERE id = 2`)
		}
		who := "alice"
		switch {
		case strings.HasSuffix(name, "_no_auth"):
			who = ""
		case strings.HasSuffix(name, "_non_admin"):
			who = "bob"
		}
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		rec := e.call(who, fx.Request.Method, fx.Request.Path, body)
		got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
		for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
			if v := rec.Header().Get(k); v != "" {
				got.Headers[k] = v
			}
		}
		replayed++
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	if replayed != 25 {
		t.Fatalf("replayed %d person fixtures, want 25", replayed)
	}
}

// Python builds the dicts in source order and json.dumps keeps it; the fixtures were recorded with sort_keys.
func TestPersonResponseKeyOrderAndNoEscaping(t *testing.T) {
	e := newPersonEnv(t, nil)
	e.seedContract(t)
	e.insert(t, personSeedRow{url: "https://www.linkedin.com/in/x", user: "x", name: "A <b> & c", headline: "h<i>", status: "completed",
		articles: json.RawMessage(`[{"url":"u","title":"t&t"}]`), social: json.RawMessage(`{"z":"1","a":"2"}`)})

	d := e.call("alice", "GET", "/api/person/1", "").Body.String()
	want := `{"id":1,"linkedin_url":"https://www.linkedin.com/in/jane-partner","username":"jane-partner","name":"Jane Partner",` +
		`"headline":"VP Partnerships at Stripe","location":"San Francisco, CA","summary":"Leads strategic partnerships.",` +
		`"emails":["jane.partner@stripe.example"],"social_links":{"github":"https://github.com/jane-partner"},` +
		`"articles":[{"title":"Partnering at scale","url":"https://example.com/a"}],"mentions":[{"title":"Podcast","url":"https://example.com/m"}],` +
		`"companies":["Stripe"],"education":["Stanford University"],"skills":["Partnerships","Payments"],` +
		`"raw_sources":[{"source":"ddg","query":"jane-partner"}],"status":"completed","created_at":"2026-08-28T12:00:00"}`
	if d != want {
		t.Errorf("detail bytes\n got %s\nwant %s", d, want)
	}
	d4 := e.call("alice", "GET", "/api/person/4", "").Body.String()
	for _, w := range []string{`"name":"A <b> & c"`, `"headline":"h<i>"`, `"social_links":{"z":"1","a":"2"}`, `"articles":[{"url":"u","title":"t&t"}]`} {
		if !strings.Contains(d4, w) {
			t.Errorf("detail 4 lacks %s in %s", w, d4)
		}
	}
	if strings.Contains(d4, `\u003`) || strings.Contains(d4, "\\u0026") {
		t.Errorf("detail is HTML-escaped: %s", d4)
	}

	h := e.call("alice", "GET", "/api/person/history", "").Body.String()
	if !strings.HasPrefix(h, `[{"id":4,"linkedin_url":"https://www.linkedin.com/in/x","username":"x","name":"A <b> & c","headline":"h<i>","email_count":0,"status":"completed","created_at":"2026-08-28T12:00:00"},{"id":3,`) {
		t.Errorf("history bytes %s", h)
	}
	if strings.Contains(h, `\u003`) || strings.Contains(h, "\\u0026") {
		t.Errorf("history is HTML-escaped: %s", h)
	}
	if !strings.Contains(h, `"name":"Émilie D."`) || strings.Contains(h, `\u00`) {
		t.Errorf("history escapes non-ASCII: %s", h)
	}
}

func TestPersonNonAdmin403(t *testing.T) {
	e := newPersonEnv(t, func(p *personRoutes) {
		p.enrich = func(context.Context, personintel.Deps, string, bool) (personintel.Profile, error) {
			t.Error("enrich ran for a non-admin")
			return personintel.Profile{}, nil
		}
	})
	e.seedContract(t)
	for _, c := range [][3]string{
		{"POST", "/api/person/enrich", `{"linkedin_url":"https://www.linkedin.com/in/x"}`},
		{"GET", "/api/person/history", ""}, {"GET", "/api/person/1", ""}, {"DELETE", "/api/person/1", ""},
	} {
		if rec := e.call("bob", c[0], c[1], c[2]); rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Not authorized"}` {
			t.Errorf("%s %s as non-admin: %d %s", c[0], c[1], rec.Code, rec.Body.String())
		}
		if rec := e.call("", c[0], c[1], c[2]); rec.Code != 401 {
			t.Errorf("%s %s without token: %d", c[0], c[1], rec.Code)
		}
	}
	if n := personRows(t); n != 3 {
		t.Errorf("non-admin DELETE changed rows: %d", n)
	}
}

func TestPersonEnrichInvalidURL400(t *testing.T) {
	e := newPersonEnv(t, func(p *personRoutes) {
		p.enrich = func(context.Context, personintel.Deps, string, bool) (personintel.Profile, error) {
			t.Error("enrich ran for an invalid URL")
			return personintel.Profile{}, nil
		}
	})
	const want = `{"detail":"Invalid LinkedIn URL. Expected format: https://www.linkedin.com/in/username"}`
	for _, u := range []string{"", "https://example.com/x", "https://www.linkedin.com/company/x", "https://www.LinkedIn.com/in/x", "linkedin.com/profile/in/x"} {
		b, _ := json.Marshal(map[string]any{"linkedin_url": u})
		rec := e.call("alice", "POST", "/api/person/enrich", string(b))
		if rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%q: %d %s", u, rec.Code, rec.Body.String())
		}
	}
	if n := personRows(t); n != 0 {
		t.Errorf("%d rows saved for invalid URLs", n)
	}
}

func TestPersonEnrichBodyValidation422(t *testing.T) {
	e := newPersonEnv(t, nil)
	for _, c := range []struct{ body, want string }{
		{`{"linkedin_url": 5}`, `"type":"string_type"`},
		{`{"linkedin_url": "https://www.linkedin.com/in/x", "deep": "maybe"}`, `"type":"bool_parsing"`},
		{`{"linkedin_url": "https://www.linkedin.com/in/x", "deep": null}`, `"type":"bool_type"`},
		{`[1]`, `"type":"model_attributes_type"`},
		{`{`, `"type":"json_invalid"`},
		{``, `"type":"json_invalid"`},
	} {
		rec := e.call("alice", "POST", "/api/person/enrich", c.body)
		if rec.Code != 422 || !strings.Contains(strings.ReplaceAll(rec.Body.String(), ` `, ``), c.want) {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body.String())
		}
	}
	if n := personRows(t); n != 0 {
		t.Errorf("%d rows saved", n)
	}
}

func TestPersonEnrichSavesAndReturnsProfile(t *testing.T) {
	e := newPersonEnv(t, nil)
	rec := e.call("alice", "POST", "/api/person/enrich", `{"linkedin_url":"https://www.linkedin.com/in/jane-doe/","deep":"yes"}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	s := rec.Body.String()
	if !strings.HasPrefix(s, `{"id":1,"status":"success","profile":{"linkedin_url":"https://www.linkedin.com/in/jane-doe/","username":"jane-doe",`) {
		t.Fatalf("body %s", s)
	}
	if !strings.Contains(s, `"status":"completed"`) {
		t.Errorf("profile not completed: %s", s)
	}
	var det struct {
		Username string `json:"username"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(e.call("alice", "GET", "/api/person/1", "").Body.Bytes(), &det); err != nil || det.Username != "jane-doe" || det.Status != "completed" {
		t.Errorf("saved record %+v %v", det, err)
	}
}

// Python's service swallows the ValueError of an unparseable username: the failed profile is saved and returned 200.
func TestPersonEnrichUnparseableUsernameSavesFailedProfile(t *testing.T) {
	e := newPersonEnv(t, nil)
	rec := e.call("alice", "POST", "/api/person/enrich", `{"linkedin_url":"https://www.linkedin.com/in/%%%"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"success"`) || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := personRows(t); n != 1 {
		t.Errorf("%d rows", n)
	}
}

func TestPersonEnrichPassesDeepFlag(t *testing.T) {
	var got []bool
	e := newPersonEnv(t, func(p *personRoutes) {
		p.enrich = func(_ context.Context, _ personintel.Deps, u string, deep bool) (personintel.Profile, error) {
			got = append(got, deep)
			pr := personintel.NewProfile(u)
			pr.Username = "x"
			pr.Status = "completed"
			return *pr, nil
		}
	})
	for _, b := range []string{`{"linkedin_url":"https://www.linkedin.com/in/x"}`, `{"linkedin_url":"https://www.linkedin.com/in/x","deep":true}`,
		`{"linkedin_url":"https://www.linkedin.com/in/x","deep":"on"}`, `{"linkedin_url":"https://www.linkedin.com/in/x","deep":0}`} {
		if rec := e.call("alice", "POST", "/api/person/enrich", b); rec.Code != 200 {
			t.Fatalf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
	}
	if fmt.Sprint(got) != "[false true true false]" {
		t.Errorf("deep flags %v", got)
	}
}

// Python answers 500 with str(e); Go never echoes an internal error text (deviation).
func TestPersonEnrichErrorNotEchoed(t *testing.T) {
	const secret = "dial tcp 10.0.0.5:5432: password=hunter2"
	e := newPersonEnv(t, func(p *personRoutes) {
		p.enrich = func(context.Context, personintel.Deps, string, bool) (personintel.Profile, error) {
			return personintel.Profile{}, errors.New(secret)
		}
	})
	rec := e.call("alice", "POST", "/api/person/enrich", `{"linkedin_url":"https://www.linkedin.com/in/x"}`)
	if rec.Code != 500 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Enrichment failed"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(e.logs.String(), "hunter2") {
		t.Errorf("log leaks the error text: %s", e.logs.String())
	}
	if n := personRows(t); n != 0 {
		t.Errorf("%d rows saved after a failed enrichment", n)
	}
}

func TestPersonHistoryPagingAndOrder(t *testing.T) {
	e := newPersonEnv(t, nil)
	for i := 1; i <= 35; i++ {
		e.insert(t, personSeedRow{url: fmt.Sprintf("https://www.linkedin.com/in/p%d", i), user: fmt.Sprintf("p%d", i), name: fmt.Sprintf("P %d", i),
			emails: make([]string, i%3), status: "completed"})
	}
	ids := func(q string) []int {
		rec := e.call("alice", "GET", "/api/person/history"+q, "")
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		var rows []struct {
			ID         int `json:"id"`
			EmailCount int `json:"email_count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		out := []int{}
		for _, r := range rows {
			if r.EmailCount != r.ID%3 {
				t.Errorf("row %d email_count %d", r.ID, r.EmailCount)
			}
			out = append(out, r.ID)
		}
		return out
	}
	if g := ids(""); len(g) != 30 || g[0] != 35 || g[29] != 6 {
		t.Errorf("default page %v", g)
	}
	if g := ids("?skip=30"); fmt.Sprint(g) != "[5 4 3 2 1]" {
		t.Errorf("skip=30 %v", g)
	}
	if g := ids("?skip=2&limit=3"); fmt.Sprint(g) != "[33 32 31]" {
		t.Errorf("skip=2&limit=3 %v", g)
	}
	if g := ids("?limit=0"); len(g) != 0 {
		t.Errorf("limit=0 %v", g)
	}
	if rec := e.call("alice", "GET", "/api/person/history?limit=0", ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("empty page body %q", rec.Body.String())
	}
	if g := ids("?skip=1000"); len(g) != 0 {
		t.Errorf("skip past end %v", g)
	}
	if g := ids("?limit=99999999999"); len(g) != 35 {
		t.Errorf("huge limit %v", g)
	}
	// Python hands a negative offset or limit to Postgres (500); Go answers 422 (deviation).
	for _, q := range []string{"?skip=-1", "?limit=-1"} {
		rec := e.call("alice", "GET", "/api/person/history"+q, "")
		name := strings.TrimPrefix(strings.Split(q, "=")[0], "?")
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"type":"greater_than_equal"`) || !strings.Contains(rec.Body.String(), `"`+name+`"`) {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	if rec := e.call("alice", "GET", "/api/person/history?skip=x&limit=y", ""); rec.Code != 422 || strings.Count(rec.Body.String(), "int_parsing") != 2 {
		t.Errorf("two bad ints: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPersonHistoryLiteralBeatsID(t *testing.T) {
	e := newPersonEnv(t, nil)
	e.seedContract(t)
	if rec := e.call("alice", "GET", "/api/person/history", ""); rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "[") {
		t.Errorf("history: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.call("alice", "GET", "/api/person/1", ""); rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "{") {
		t.Errorf("detail: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPersonDetailNotFound404(t *testing.T) {
	e := newPersonEnv(t, nil)
	e.seedContract(t)
	for _, id := range []string{"999", "0", "-5", "2147483648", "99999999999999999999"} {
		rec := e.call("alice", "GET", "/api/person/"+id, "")
		if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Record not found"}` {
			t.Errorf("GET %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
}

func TestPersonDeleteTwice404(t *testing.T) {
	e := newPersonEnv(t, nil)
	e.seedContract(t)
	rec := e.call("alice", "DELETE", "/api/person/2", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"deleted","id":2}` {
		t.Fatalf("first delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.call("alice", "DELETE", "/api/person/2", "")
	if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Record not found"}` {
		t.Errorf("second delete: %d %s", rec.Code, rec.Body.String())
	}
	if n := personRows(t); n != 2 {
		t.Errorf("%d rows left", n)
	}
	if rec := e.call("alice", "DELETE", "/api/person/2147483648", ""); rec.Code != 404 {
		t.Errorf("out-of-range delete: %d", rec.Code)
	}
}

func TestPersonRecordIDNotInteger422(t *testing.T) {
	e := newPersonEnv(t, nil)
	e.seedContract(t)
	for _, m := range []string{"GET", "DELETE"} {
		for _, id := range []string{"abc", "1.5", "1e3", "history2", "0x1"} {
			rec := e.call("alice", m, "/api/person/"+id, "")
			var out struct {
				Detail []struct {
					Type string `json:"type"`
					Loc  []any  `json:"loc"`
					Msg  string `json:"msg"`
				} `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 422 || len(out.Detail) != 1 ||
				out.Detail[0].Type != "int_parsing" || fmt.Sprint(out.Detail[0].Loc) != "[path record_id]" {
				t.Errorf("%s %s: %d %s", m, id, rec.Code, rec.Body.String())
			}
		}
	}
	if n := personRows(t); n != 3 {
		t.Errorf("%d rows", n)
	}
}

// Deep scrape needs the caller's workspace: the worker rejects an empty workspace_id.
func TestPersonEnrichPassesCallerWorkspace(t *testing.T) {
	var got []string
	e := newPersonEnv(t, func(p *personRoutes) {
		p.enrich = func(_ context.Context, pd personintel.Deps, u string, _ bool) (personintel.Profile, error) {
			got = append(got, pd.WorkspaceID)
			pr := personintel.NewProfile(u)
			pr.Username = "x"
			pr.Status = "completed"
			return *pr, nil
		}
	})
	body := `{"linkedin_url":"https://www.linkedin.com/in/x","deep":true}`
	call := func(hdr map[string]string) int {
		h := bearer("alice")
		for k, v := range hdr {
			h[k] = v
		}
		rec, _ := e.do("POST", "/api/person/enrich", "application/json", body, h)
		return rec.Code
	}
	call(nil)                                             // alice's active workspace
	call(map[string]string{"X-Workspace-Id": "ws-alpha"}) // explicit, a member
	call(map[string]string{"X-Workspace-Id": "ws-beta"})  // not a member: never forwarded
	if len(got) != 3 || got[0] != "ws-alpha" || got[1] != "ws-alpha" || got[2] != "ws-alpha" {
		t.Errorf("workspace ids %v, want ws-alpha x3", got)
	}
	if w := personintel.DefaultWorkspaceID; w == "" {
		t.Error("empty default workspace")
	}
}

func TestPersonWorkspaceIDFallsBackToSentinel(t *testing.T) {
	e := newPersonEnv(t, nil)
	ownerExec(t, `DELETE FROM workspace_members WHERE user_id = (SELECT id FROM users WHERE username = 'alice')`)
	ownerExec(t, `UPDATE workspaces SET owner_id = NULL WHERE owner_id = (SELECT id FROM users WHERE username = 'alice')`)
	u := e.mustUserByName(t, "alice")
	if got := PersonIntelWorkspaceID(context.Background(), Deps{Identity: e.st}, u.ID, ""); got != personintel.DefaultWorkspaceID {
		t.Errorf("no workspace: %q", got)
	}
}
