//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionbox"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const abLeak = "SECRET-UPSTREAM-MARKER"

// fakeGateway is the Go port of record_vendor_transcripts.py ambitionbox_server + ab_search:
// the gateway the contract fixtures were recorded against. It is a RoundTripper, no socket.
type fakeGateway struct {
	calls int
	block chan struct{} // when set, every request waits for its context to end
}

func abCard(id int, name string, rating, reviews any, industry string) map[string]any {
	emp := ""
	if id%3 == 0 {
		emp = "1L+ Employees"
	}
	return map[string]any{
		"companyId": id, "name": name, "shortName": strings.Split(name, " ")[0], "urlName": strings.ReplaceAll(strings.ToLower(name), " ", "-"),
		"logoUrl": fmt.Sprintf("https://static.ambitionbox.com/logos/%d.png", id), "primaryIndustry": industry,
		"companyRating": rating, "reviewCount": reviews, "jobsCount": id % 7, "salariesCount": id % 11,
		"interviewsCount": id % 5, "totalEmployeesIndia": emp,
		"totalEmployees": "10k-50k Employees", "topLocationDetail": map[string]any{"name": "Pune", "totalLocationsCount": 4},
		"isVerifiedEmployer": id%2 == 0, "tag": map[string]any{"name": "Private"},
		"highlyRatedFor":     []any{map[string]any{"name": "Job Security", "ratings": 4.2}},
		"criticallyRatedFor": []any{map[string]any{"name": "Work Satisfaction", "ratings": 2.9}},
	}
}

func abSearch(body map[string]any) map[string]any {
	page := 1
	switch p := body["page"].(type) {
	case string:
		page, _ = strconv.Atoi(p)
	case float64:
		page = int(p)
	}
	inds, _ := body["industries"].([]any)
	total := 57
	switch {
	case len(inds) > 0:
		total = 31
	case body["ratings"] != nil:
		total = 25
	}
	pages := (total + 19) / 20
	industry := "IT Services"
	if len(inds) > 0 {
		parts := strings.Split(strings.ReplaceAll(inds[0].(string), "-", " "), " ")
		for i, w := range parts {
			if w != "" {
				parts[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
			}
		}
		industry = strings.Join(parts, " ")
	}
	var cards []any
	if page <= pages {
		for k := 0; k < min(20, total-(page-1)*20); k++ {
			idx := (page-1)*20 + k
			cid := 10000 + idx
			if page == 2 && k == 0 {
				cid = 10000
			}
			if idx == 5 {
				cid = 0
			}
			var rating, reviews any
			if idx%9 != 4 {
				rating = pyFloatNum(math.Round((2.5+float64(idx%25)/10)*1000) / 1000)
				reviews = 100 + idx*13
			}
			cards = append(cards, abCard(cid, fmt.Sprintf("Contract Company %03d", idx), rating, reviews, industry))
		}
	}
	return map[string]any{"cards": cards, "stats": map[string]any{"totalCards": total, "totalPages": pages}}
}

// pyFloatNum keeps a whole float written as "3.0" like Python's json.dumps (Go's encoder drops the ".0").
func pyFloatNum(f float64) json.Number {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return json.Number(s)
}

var abIDRe = regexp.MustCompile(`/company/(\d+)/|/jobs/company/(\d+)`)

func (g *fakeGateway) RoundTrip(req *http.Request) (*http.Response, error) {
	g.calls++
	if g.block != nil {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-g.block:
		}
	}
	var body map[string]any
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &body)
	}
	status, doc := g.answer(req.URL.Path, body)
	var out []byte
	if s, ok := doc.(string); ok {
		out = []byte(s)
	} else {
		out, _ = json.Marshal(doc)
	}
	return &http.Response{StatusCode: status, Status: strconv.Itoa(status), Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(out)), Request: req}, nil
}

func (g *fakeGateway) answer(path string, body map[string]any) (int, any) {
	cid := 0
	if m := abIDRe.FindStringSubmatch(path); m != nil {
		cid, _ = strconv.Atoi(m[1] + m[2])
	}
	switch {
	case cid == 429:
		return 429, "Too many requests"
	case cid == 500:
		return 503, "upstream unavailable " + abLeak
	case cid == 400:
		return 400, `{"message": "invalid company id"}`
	case strings.HasSuffix(path, "/companies/search"):
		return 200, abSearch(body)
	case strings.Contains(path, "/jobs/company/"):
		jobs := []any{}
		for k := 0; k < 3; k++ {
			maxExp := 0
			if k > 0 {
				maxExp = k + 2
			}
			jobs = append(jobs, map[string]any{"jobId": fmt.Sprintf("J%d-%d", cid, k), "title": fmt.Sprintf("Engineer %d", k),
				"company": fmt.Sprintf("Contract Company %d", cid), "companyId": cid, "companyRating": 3.84, "jobProfile": "Engineering",
				"locations": []any{"Pune"}, "minExp": k, "maxExp": maxExp, "skills": []any{"go", "sql"}, "postedOn": "2026-08-20",
				"portal": "ambitionbox", "jdpUrl": fmt.Sprintf("/jobs/engineer-%d-jdp", k)})
		}
		return 200, map[string]any{"jobs": jobs, "pagination": map[string]any{"count": 3, "currentPage": 1, "totalPages": 1}}
	case strings.HasSuffix(path, "/sectionalDetails"):
		return 200, map[string]any{"companyId": cid, "name": fmt.Sprintf("Contract Company %d", cid),
			"ratings": map[string]any{"overall": 3.9, "workLife": 3.5}, "benefits": []any{"Health insurance", "Flexible hours"}}
	case strings.HasSuffix(path, "/similar-companies-v3"):
		return 200, []any{map[string]any{"companyId": cid + 1, "name": "Similar One", "rating": 3.7},
			map[string]any{"companyId": cid + 2, "name": "Similar Two", "rating": 4.1}}
	}
	return 404, "not found"
}

type abEnv struct {
	leadsEnv
	gw     *fakeGateway
	getenv map[string]string
}

func newAbEnv(t *testing.T) *abEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	gw := &fakeGateway{}
	old := newAmbitionBoxClient
	newAmbitionBoxClient = func(Deps) *ambitionbox.Client {
		return &ambitionbox.Client{HTTP: &http.Client{Transport: gw}, Base: ambitionbox.Gateway,
			Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
	}
	t.Cleanup(func() { newAmbitionBoxClient = old })
	env := map[string]string{}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{},
		Flags:  config.Flags{PGLeadStore: true, GoConnectorRoutesEnabled: true},
		Getenv: func(k string) string { return env[k] }}
	RegisterAmbitionBox(r, d)
	e := &abEnv{leadsEnv: leadsEnv{mcpEnv{env: env0(r, st), pool: pool}}, gw: gw, getenv: env}
	e.purge()
	e.seedLeads(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	e.cleanAB()
	t.Cleanup(e.cleanAB)
	t.Cleanup(e.purge)
	e.seedWorkbooks(t)
	return e
}

func env0(r *httpx.Router, st *identity.Store) env { return env{h: r.Handler(), st: st} }

func (e *abEnv) cleanAB() {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return
	}
	defer c.Close(ctx)
	for _, q := range []string{
		`DELETE FROM jobs WHERE workspace_id IN ('ws-alpha','ws-beta') AND type = 'ambitionbox_import'`,
		`DELETE FROM connector_runs WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM workbook_rows WHERE workbook_id IN (SELECT id FROM workbooks WHERE workspace_id IN ('ws-alpha','ws-beta'))`,
		`DELETE FROM workbooks WHERE workspace_id IN ('ws-alpha','ws-beta')`,
	} {
		_, _ = c.Exec(ctx, q)
	}
}

// seedWorkbooks is contract_seed.py's workbook set: wb-alpha-1 (not AmbitionBox), wb-beta-1,
// wb-alpha-ab (import target) and wb-alpha-ab-active (one pending run).
func (e *abEnv) seedWorkbooks(t *testing.T) {
	t.Helper()
	ins := `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config, filter_criteria,
		columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd, created_at, updated_at)
		VALUES ($1,$2,'seeded','draft',$3,$4,$5::json,'{}'::json,'[]'::json,0,0,false,NULL,0,now(),now())`
	ownerExec(t, ins, "wb-alpha-1", "Alpha Empty", "ws-alpha", nil, nil)
	ownerExec(t, ins, "wb-beta-1", "Beta Workbook", "ws-beta", nil, nil)
	ownerExec(t, ins, "wb-alpha-ab", "Alpha AmbitionBox", "ws-alpha", "ambitionbox", `{"provider":"ambitionbox"}`)
	ownerExec(t, ins, "wb-alpha-ab-active", "Alpha AmbitionBox Active", "ws-alpha", "ambitionbox", `{"provider":"ambitionbox"}`)
	ownerExec(t, `INSERT INTO connector_runs (id, workspace_id, workbook_id, connector, status, query, cursor, requested_count,
		fetched_count, added_count, updated_count, skipped_count, pages_fetched, source_total, target_met, exhausted, created_at, updated_at)
		VALUES ('run-ab-active-1','ws-alpha','wb-alpha-ab-active','ambitionbox','pending',
		'{"industry": null, "rating": null, "sort_by": "popular", "max_pages": 7, "replace_existing": false}'::json,
		'{"next_page": 1, "reset_done": false, "seen_record_ids": []}'::json,40,0,0,0,0,0,NULL,false,false,now(),now())`)
}

func (e *abEnv) ownerText(t *testing.T, sql string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var s string
	if err := c.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func abActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case name == "companies_nonmember_erin_beta", name == "import_nonmember_erin_beta":
		return "erin", "ws-beta"
	case name == "import_new_with_filters":
		return "erin", "ws-alpha"
	case strings.Contains(name, "_bob_alpha"):
		return "bob", "ws-alpha"
	}
	return "alice", "ws-alpha"
}

func (e *abEnv) call(who, ws, method, path, body string) contract.Resp {
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
	_, got := e.do(method, path, ct, body, hdr)
	return got
}

// abDeviations are the fixtures Python answered 500 for; Go answers the documented 4xx/502 (D4).
var abDeviations = map[string]struct {
	status int
	body   string
}{
	"companies_location_not_implemented":     {422, `{"detail":"AmbitionBox's gateway ignores location filters -- refusing to return silently unfiltered results"}`},
	"companies_company_type_not_implemented": {422, `{"detail":"AmbitionBox's gateway ignores company_type filters -- refusing to return silently unfiltered results"}`},
	"collect_location_not_implemented":       {422, `{"detail":"AmbitionBox's gateway ignores location filters -- refusing to return silently unfiltered results"}`},
	"jobs_upstream_429":                      {502, `{"detail":"AmbitionBox upstream error"}`},
	"jobs_upstream_400":                      {502, `{"detail":"AmbitionBox upstream error"}`},
	"detail_upstream_5xx":                    {502, `{"detail":"AmbitionBox upstream error"}`},
	"similar_upstream_429":                   {502, `{"detail":"AmbitionBox upstream error"}`},
	"import_foreign_workbook":                {404, `{"detail":"workbook not found in the active workspace"}`},
	"import_unknown_workbook":                {404, `{"detail":"workbook not found in the active workspace"}`},
	"import_non_ambitionbox_without_replace": {409, `{"detail":"existing non-AmbitionBox workbooks require replace_existing=true"}`},
}

func (e *abEnv) replay(t *testing.T, name string, opts ...contract.Option) contract.Resp {
	t.Helper()
	fx := contract.Load(t, "ambitionbox/"+name)
	who, ws := abActor(name)
	got := e.call(who, ws, fx.Request.Method, fx.Request.Path, fixtureBody(fx))
	if dev, ok := abDeviations[name]; ok {
		if got.Status != dev.status {
			t.Errorf("%s: status %d, want %d (%s)", name, got.Status, dev.status, got.Body)
		}
		if strings.ReplaceAll(string(got.Body), " ", "") != strings.ReplaceAll(dev.body, " ", "") && string(got.Body) != dev.body {
			t.Errorf("%s: body %s, want %s", name, got.Body, dev.body)
		}
		return got
	}
	if fx.Response.Status == 202 { // ids come from this process, not the recorder's patched uuid4 and serial
		opts = append(opts, contract.IgnorePaths("$.job_id", "$.run_id"))
		if strings.Contains(string(fx.Response.Body), `"workbook_id":"00000000-`) || strings.Contains(string(fx.Response.Body), `"workbook_id": "00000000-`) {
			opts = append(opts, contract.IgnorePaths("$.workbook_id", "$.url"))
		}
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
	return got
}

func (e *abEnv) replayAll(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		e.replay(t, n)
	}
}

func TestCompaniesMatchesFixture(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "companies_default", "companies_viewer_bob_alpha", "companies_filters", "companies_industry_alias", "companies_beyond_last_page")
	// key order is Python's wire order (fixtures are key-sorted): the card keys and the envelope.
	got := e.call("alice", "ws-alpha", "GET", "/api/ambitionbox/companies?limit=1", "")
	if !strings.HasPrefix(string(got.Body), `{"companies":[{"company_id":10000,"name":"Contract Company 000"`) ||
		!strings.Contains(string(got.Body), `],"total":1,"page":1,"source_total":57,"total_pages":3,"has_more":true}`) {
		t.Fatalf("key order: %s", got.Body)
	}
}

func TestCompaniesLimitBounds422(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "companies_limit_0", "companies_limit_21", "companies_page_0")
	before := e.gw.calls
	for _, p := range []string{"limit=abc", "page=1.5", "page=99999999999999999999"} {
		if got := e.call("alice", "ws-alpha", "GET", "/api/ambitionbox/companies?"+p, ""); got.Status != 422 {
			t.Errorf("%s: %d %s", p, got.Status, got.Body)
		}
	}
	if e.gw.calls != before {
		t.Fatal("an invalid query reached the gateway")
	}
}

func TestLocationFilterIs422NotCrash(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "companies_location_not_implemented", "companies_company_type_not_implemented", "collect_location_not_implemented")
	if e.gw.calls != 0 {
		t.Fatalf("unsupported filters reached the gateway (%d calls)", e.gw.calls)
	}
	// an empty value is no filter (Python: `x.split(",") if x else None`)
	if got := e.call("alice", "ws-alpha", "GET", "/api/ambitionbox/companies?location=&company_type=", ""); got.Status != 200 {
		t.Fatalf("empty filters: %d %s", got.Status, got.Body)
	}
}

func TestJobsDetailSimilarMatchFixtures(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "jobs_ok", "jobs_no_auth", "detail_ok", "detail_no_auth", "similar_ok",
		"jobs_upstream_429", "jobs_upstream_400", "detail_upstream_5xx", "similar_upstream_429")
	got := e.call("alice", "ws-alpha", "GET", "/api/ambitionbox/companies/777/similar", "")
	if !strings.HasPrefix(string(got.Body), `[{`) {
		t.Fatalf("similar must be a JSON list: %s", got.Body)
	}
	// a negative page is passed through (Python `page: int = 1` has no bound)
	if got := e.call("alice", "ws-alpha", "GET", "/api/ambitionbox/companies/777/jobs?page=-1", ""); got.Status != 200 {
		t.Fatalf("page=-1: %d %s", got.Status, got.Body)
	}
}

func TestPathIDNotIntegerIs422(t *testing.T) {
	e := newAbEnv(t)
	e.replay(t, "jobs_id_not_int")
	for _, p := range []string{"detail", "similar"} {
		if got := e.call("alice", "ws-alpha", "GET", "/api/ambitionbox/companies/abc/"+p, ""); got.Status != 422 ||
			!strings.Contains(string(got.Body), `"loc":["path","company_id"]`) {
			t.Errorf("%s: %d %s", p, got.Status, got.Body)
		}
	}
	if e.gw.calls != 0 {
		t.Fatal("a bad id reached the gateway")
	}
}

func TestCollectMatchesFixture(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "collect_default", "collect_target_met", "collect_max_pages", "collect_industry")
	got := e.call("alice", "ws-alpha", "GET", "/api/ambitionbox/collect?pages=1&limit=5", "")
	if !strings.HasPrefix(string(got.Body), `{"provider":"ambitionbox","requested_count":5,"returned_count":5,"pages_fetched":1,`) ||
		!strings.Contains(string(got.Body), `"warnings":[],"companies":[{`) || !strings.HasSuffix(string(got.Body), `],"total":5}`) {
		t.Fatalf("collect key order: %.400s", got.Body)
	}
}

func TestCollectBounds422(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "collect_pages_0", "collect_pages_26", "collect_limit_0", "collect_limit_501")
	if e.gw.calls != 0 {
		t.Fatal("an invalid query reached the gateway")
	}
}

func TestAmbitionBoxReadsRequireWorkspace(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "companies_no_auth", "companies_nonmember_erin_beta", "collect_no_auth", "jobs_no_auth", "detail_no_auth")
	if e.gw.calls != 0 {
		t.Fatalf("unauthenticated or non-member requests reached the gateway: %d", e.gw.calls)
	}
	for _, p := range []string{"/companies", "/companies/1/jobs", "/companies/1/detail", "/companies/1/similar", "/collect"} {
		if got := e.call("alice", "", "GET", "/api/ambitionbox"+p, ""); got.Status < 400 && got.Status != 200 {
			t.Errorf("%s without workspace header: %d", p, got.Status)
		}
	}
}

func TestUpstreamErrorIs502NoBodyLeak(t *testing.T) {
	e := newAbEnv(t)
	for _, p := range []string{"/companies/500/jobs", "/companies/500/detail", "/companies/500/similar", "/companies/429/jobs", "/companies/400/detail"} {
		got := e.call("alice", "ws-alpha", "GET", "/api/ambitionbox"+p, "")
		if got.Status != 502 || string(got.Body) != `{"detail":"AmbitionBox upstream error"}` || strings.Contains(string(got.Body), abLeak) {
			t.Errorf("%s: %d %s", p, got.Status, got.Body)
		}
	}
}

func TestReadsHonourRequestContext(t *testing.T) {
	e := newAbEnv(t)
	e.gw.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/ambitionbox/companies", nil).WithContext(ctx)
	for k, v := range bearer("alice") {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Workspace-Id", "ws-alpha")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { e.h.ServeHTTP(rec, req); close(done) }()
	time.AfterFunc(100*time.Millisecond, cancel)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler ignored the cancelled request context")
	}
	if rec.Code != 502 {
		t.Fatalf("cancelled read: %d %s", rec.Code, rec.Body)
	}
}

func TestImportsAcceptedMatchesFixture(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "import_defaults")
	e.replayAll(t, "import_new_with_filters")
	e.replayAll(t, "import_blank_name_limit_1")
}

func TestImportsRunningIs409WithRunID(t *testing.T) {
	e := newAbEnv(t)
	e.replay(t, "import_seeded_pending_run_conflict")
	first := e.replay(t, "import_existing_ambitionbox_workbook")
	var started struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(first.Body, &started)
	again := e.replay(t, "import_existing_again_conflict", contract.IgnorePaths("$.detail.message", "$.detail.run_id"))
	want := fmt.Sprintf(`{"detail":{"code":"connector_run_active","message":"AmbitionBox import %[1]s is already active","run_id":"%[1]s"}}`, started.RunID)
	if string(again.Body) != want {
		t.Fatalf("conflict body %s, want %s", again.Body, want)
	}
}

func TestImportsForeignWorkbookIs404(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "import_foreign_workbook", "import_unknown_workbook")
	if n := e.ownerText(t, `SELECT count(*)::text FROM connector_runs WHERE workbook_id IN ('wb-beta-1','wb-does-not-exist')`); n != "0" {
		t.Fatalf("a rejected import left %s runs", n)
	}
	if n := e.ownerText(t, `SELECT count(*)::text FROM workbooks WHERE id = 'wb-beta-1' AND source_type IS NOT NULL`); n != "0" {
		t.Fatal("a foreign workbook was repurposed")
	}
}

func TestImportsNonAmbitionBoxNeedsReplace(t *testing.T) {
	e := newAbEnv(t)
	e.replay(t, "import_non_ambitionbox_without_replace")
	if st := e.ownerText(t, `SELECT coalesce(source_type,'none') FROM workbooks WHERE id='wb-alpha-1'`); st != "none" {
		t.Fatalf("a refused import changed the workbook: %s", st)
	}
	e.replay(t, "import_non_ambitionbox_with_replace")
	if st := e.ownerText(t, `SELECT source_type FROM workbooks WHERE id='wb-alpha-1'`); st != "ambitionbox" {
		t.Fatalf("replace_existing did not repurpose: %s", st)
	}
}

func TestImportsViewerForbidden(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "import_viewer_bob_alpha", "import_no_auth", "import_nonmember_erin_beta")
	if n := e.ownerText(t, `SELECT count(*)::text FROM jobs WHERE type='ambitionbox_import' AND workspace_id IN ('ws-alpha','ws-beta')`); n != "0" {
		t.Fatalf("a forbidden import enqueued %s jobs", n)
	}
}

func TestImportsSortByPattern422(t *testing.T) {
	e := newAbEnv(t)
	e.replayAll(t, "import_bad_sort", "import_limit_0", "import_limit_501", "import_bad_types")
	for _, b := range []string{`{"limit":true}`, `{"limit":"x"}`, `{"limit":1.5}`, `{"replace_existing":[1]}`, `{"sort_by":5}`, `{"workbook_id":3}`, `[]`, ``} {
		if got := e.call("alice", "ws-alpha", "POST", "/api/ambitionbox/imports", b); got.Status != 422 {
			t.Errorf("%q: %d %.300s", b, got.Status, got.Body)
		}
	}
	// lax coercions pydantic accepts: numeric strings, 1.0 and "true"
	for _, b := range []string{`{"limit":"5"}`, `{"limit":5.0,"replace_existing":"true"}`, `{"replace_existing":1}`} {
		if got := e.call("alice", "ws-alpha", "POST", "/api/ambitionbox/imports", b); got.Status != 202 {
			t.Errorf("%q: %d %s", b, got.Status, got.Body)
		}
	}
	if n := e.ownerText(t, `SELECT count(*)::text FROM connector_runs WHERE workspace_id='ws-alpha' AND id <> 'run-ab-active-1'`); n != "3" {
		t.Fatalf("only the three accepted bodies may start a run, got %s (%s)", n, e.ownerText(t, `SELECT string_agg(requested_count::text||':'||workbook_id, ',') FROM connector_runs WHERE workspace_id='ws-alpha' AND id <> 'run-ab-active-1'`))
	}
}

func TestUpstreamErrorNeverFromImports(t *testing.T) {
	e := newAbEnv(t)
	e.replay(t, "import_defaults")
	if e.gw.calls != 0 {
		t.Fatal("POST /imports must not call the gateway (the job does)")
	}
}

func TestImportsEnqueueExecutorFlag(t *testing.T) {
	e := newAbEnv(t)
	run := func() string {
		got := e.call("alice", "ws-alpha", "POST", "/api/ambitionbox/imports", `{"limit":5}`)
		if got.Status != 202 {
			t.Fatalf("import: %d %s", got.Status, got.Body)
		}
		var res struct {
			JobID int64 `json:"job_id"`
		}
		_ = json.Unmarshal(got.Body, &res)
		return e.ownerText(t, `SELECT executor FROM jobs WHERE id=$1`, res.JobID)
	}
	if ex := run(); ex != "python" {
		t.Fatalf("default executor %q", ex)
	}
	e.getenv["GO_EXECUTOR_ENABLED"], e.getenv["GO_EXECUTOR_AMBITIONBOX"], e.getenv["GO_EXECUTOR_WORKSPACES"] = "true", "true", "ws-beta"
	if ex := run(); ex != "python" {
		t.Fatalf("workspace outside the cohort got %q", ex)
	}
	e.getenv["GO_EXECUTOR_WORKSPACES"] = "ws-beta, ws-alpha"
	if ex := run(); ex != "go" {
		t.Fatalf("cohort workspace got %q", ex)
	}
}
