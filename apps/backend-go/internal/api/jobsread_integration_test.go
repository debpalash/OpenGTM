//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Plan 14 Task 15: the jobs read routes replayed against the recorded Python fixtures (testdata/contract/jobs).
// The seed mirrors legacy/python/scripts/contract_seed.py (_seed_leadgen): the job rows are read back from the
// recorded list fixtures, so the stored intent_details text is exactly what Python stored.

const (
	jA1 = "a1000000000000000000000000000001"
	jA2 = "a1000000000000000000000000000002"
	jA3 = "a1000000000000000000000000000003"
	jA4 = "a1000000000000000000000000000004"
	jA5 = "a1000000000000000000000000000005"
	jA7 = "a1000000000000000000000000000007"
	jB1 = "b1000000000000000000000000000001"
	jB2 = "b1000000000000000000000000000002"
)

type jobsEnv struct {
	mcpEnv
	rdb *redis.Client
}

func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opt)
	t.Cleanup(func() { c.Close() })
	return c
}

// newJobsEnv builds the router with rdb (nil: no Redis) and seeds users, workspaces, jobs, stages and leads.
func newJobsEnv(t *testing.T, rdb *redis.Client) jobsEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Redis: rdb,
		Flags:                   config.Flags{PGLeadStore: true, Leadgen: config.LeadgenSettings{GoCollectionRoutesEnabled: true}},
		CollectionLedgerBackend: "postgres"}
	// the shared progress bus (collectroute.go) is process-wide: point it at this env's Redis
	bus := leadprogress.NewBus(rdb)
	collectBus = func(Deps) *leadprogress.Bus { return bus }
	t.Cleanup(func() { collectBus = nil })
	RegisterJobs(r, d)
	e := jobsEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, rdb: rdb}
	e.purge(t)
	t.Cleanup(func() { e.purge(t) })
	e.seedAll(t)
	return e
}

func (e jobsEnv) purge(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, "DELETE FROM collection_jobs") // cascades stages
			_, err := tx.Exec(ctx, "DELETE FROM leads")
			return err
		})
		if e.rdb != nil {
			e.rdb.Del(ctx, "leadgen:progress:"+ws+":recent")
		}
	}
}

func fixtureRows(t *testing.T, name string) []map[string]any {
	t.Helper()
	fx := contract.Load(t, "jobs/"+name)
	var rows []map[string]any
	if err := json.Unmarshal(fx.Response.Body, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func (e jobsEnv) seedAll(t *testing.T) {
	t.Helper()
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	ctx := context.Background()

	for ws, fixture := range map[string]string{"ws-alpha": "jobs_list_default", "ws-beta": "jobs_list_beta"} {
		rows := fixtureRows(t, fixture)
		var cols []string
		for k := range rows[0] {
			cols = append(cols, k)
		}
		list := strings.Join(cols, ", ")
		b, _ := json.Marshal(rows)
		err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO collection_jobs (%[1]s) SELECT %[1]s FROM json_populate_recordset(null::collection_jobs, $1::json)`, list), string(b))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	ownerExec(t, `SELECT setval(pg_get_serial_sequence('collection_job_stages','id'), 1, false)`)
	ts := func(d, h, m, s, us int) string {
		out := fmt.Sprintf("2026-08-%02d %02d:%02d:%02d", d, h, m, s)
		out = strings.Replace(out, " ", "T", 1)
		if us != 0 {
			out += fmt.Sprintf(".%06d", us)
		}
		return out + "+00:00"
	}
	type stage struct {
		job, ws, name, status string
		in, out, rej          int
		details, start, end   string
	}
	var stages []stage
	details := map[int][2]string{
		1: {`{}`, `{}`},
		2: {`{"sources": ["web_search", "directory"], "samples": ["Acme HR", "Zeta Staffing"]}`, `{"reasons": {}}`},
		3: {`{"reasons": {"no_website": 2}}`, `{"cross_job_removed": 1, "in_batch_removed": 0}`},
		4: {`{"note": "東京 ✓"}`, `{"cross_job_removed": 0}`},
		5: {`{"error": "Timed out after 60s"}`, `{}`},
		6: {`{"reasons": {}}`, `{"cross_job_removed": 0}`},
		7: {`not-json{`, ``},
	}
	sts := map[int][2]string{1: {"running", "running"}, 2: {"done", "running"}, 3: {"done", "done"}, 4: {"done", "done"},
		5: {"failed", "running"}, 6: {"done", "running"}, 7: {"running", "running"}}
	for n := 1; n <= 7; n++ {
		id := fmt.Sprintf("a1%030x", n)
		d, s := details[n], sts[n]
		end1, end2 := "", ""
		if s[0] != "running" {
			end1 = ts(20+n, 9, 1, 0, 6)
		}
		if s[1] != "running" {
			end2 = ts(20+n, 9, 2, 0, 8)
		}
		stages = append(stages,
			stage{id, "ws-alpha", "validate", s[0], 12 + n, 10 + n, 2, d[0], ts(20+n, 9, 0, 0, 5), end1},
			stage{id, "ws-alpha", "dedup", s[1], 10 + n, 9 + n, 1, d[1], ts(20+n, 9, 1, 0, 7), end2})
	}
	for n := 1; n <= 2; n++ {
		id := fmt.Sprintf("b1%030x", n)
		st2, end2, out2 := "running", "", 0
		if n == 1 {
			st2, end2, out2 = "done", ts(27, 7, 3, 5, 0), 4
		}
		stages = append(stages,
			stage{id, "ws-beta", "validate", "done", 5, 4, 1, `{"reasons": {"no_email": 1}}`, ts(27, 7, 0, 5, 0), ts(27, 7, 1, 5, 0)},
			stage{id, "ws-beta", "dedup", st2, 4, out2, 0, `{}`, ts(27, 7, 2, 5, 0), end2})
	}
	for _, s := range stages {
		err := tenancy.InWorkspace(ctx, e.pool, s.ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO collection_job_stages (job_id, workspace_id, stage, status, input_count, output_count, rejected_count, details, started_at, completed_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, s.job, s.ws, s.name, s.status, s.in, s.out, s.rej, s.details, s.start, s.end)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	leads := []struct {
		id                               int
		ws, cj, company, city, web, mail string
		phone, source                    string
		score                            int
		tier, status, created            string
	}{
		{101, "ws-alpha", jA3, "Mumbai Talent Partners", "Mumbai", "https://mtp.example.com", "hr@mtp.example.com", "+91 22 5550 2001", "web_search", 85, "hot", "new", "2026-08-28T09:05:00"},
		{102, "ws-alpha", jA3, "Konkan Recruiters", "Mumbai", "https://konkan.example.com", "", "", "web_search", 60, "warm", "new", "2026-08-28T09:06:00"},
		{103, "ws-alpha", jA3, "Harbour Staffing", "Navi Mumbai", "", "", "", "directory", 30, "cold", "contacted", "2026-08-28T09:07:00"},
		{104, "ws-alpha", "", "東京ソリューションズ", "東京", "https://tokyo-sol.example.com", "", "", "job:" + jA4, 55, "warm", "new", "2026-08-27T10:10:00"},
		{105, "ws-alpha", jA4, "Café Systems KK", "東京", "", "ops@cafe-sys.example.com", "", "job:" + jA4, 20, "unqualified", "new", "2026-08-27T10:11:00"},
		{106, "ws-beta", jB1, "Beta Staffing Co", "Chennai", "https://betastaff.example.com", "", "", "web_search", 45, "cold", "new", "2026-08-28T07:04:00"},
	}
	for _, l := range leads {
		err := tenancy.InWorkspace(ctx, e.pool, l.ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO leads (id, workspace_id, collection_job_id, company, city, website, email, phone, source, score, score_tier, status, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)`, l.id, l.ws, l.cj, l.company, l.city, l.web, l.mail, l.phone, l.source, l.score, l.tier, l.status, l.created)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('leads','id'),
		GREATEST(1000, (SELECT COALESCE(max(id), 0) FROM leads), (SELECT last_value FROM leads_id_seq)))`)
}

// seedProgress writes the Redis history of _seed_leadgen_progress (LPUSH, newest first).
func (e jobsEnv) seedProgress(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	ev := func(offset float64, typ, ws string, data map[string]any) string {
		// Python's repr of a float keeps ".0" on whole numbers
		ts := strconv.FormatFloat(1788000000.0+offset, 'f', -1, 64)
		if !strings.Contains(ts, ".") {
			ts += ".0"
		}
		m := map[string]any{"type": typ, "ts": json.Number(ts), "workspace_id": ws}
		for k, v := range data {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	d := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	alpha := []string{
		ev(0, "job_created", "ws-alpha", d("job_id", jA2, "query", "IT staffing companies in Bangalore", "message", "Collection queued: IT staffing companies in Bangalore")),
		ev(1.25, "stage_start", "ws-alpha", d("job_id", jA2, "stage", "discovery", "message", "Discovering sources")),
		ev(2.5, "source_progress", "ws-alpha", d("job_id", jA2, "source", "web_search", "found", 7)),
		ev(3, "stage_start", "ws-alpha", d("job_id", jA3, "message", "Another job's event")),
		ev(3.75, "stage_done", "ws-alpha", d("job_id", jA2, "stage", "discovery", "message", "Found 12 leads — “IT staffing” ✓")),
		ev(5, "lead_found", "ws-alpha", d("job_id", jA2, "message", "Added Acme HR", "count", 1)),
		ev(6.5, "stage_start", "ws-alpha", d("job_id", jA2, "stage", "validate", "message", "Validating 12 leads")),
	}
	beta := []string{
		ev(0, "job_created", "ws-beta", d("job_id", jB2, "message", "Collection queued: beta recruiting Chennai")),
		ev(1, "stage_start", "ws-beta", d("job_id", jB2, "stage", "validate", "message", "Validating")),
	}
	for ws, rows := range map[string][]string{"ws-alpha": alpha, "ws-beta": beta} {
		key := "leadgen:progress:" + ws + ":recent"
		e.rdb.Del(ctx, key)
		for _, r := range rows {
			e.rdb.LPush(ctx, key, r)
		}
	}
}

// jobsActor says who sent a recorded request (the recorder redacts Authorization and X-Workspace-Id).
func jobsActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case name == "jobs_list_nonmember_erin_beta":
		return "erin", "ws-beta"
	case strings.HasSuffix(name, "_beta"), name == "jobs_list_beta_final":
		return "bob", "ws-beta"
	case name == "jobs_list_viewer_bob_alpha":
		return "bob", "ws-alpha"
	}
	return "alice", ""
}

func (e jobsEnv) get(who, ws, path string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do("GET", path, "", "", hdr)
	return rec
}

func (e jobsEnv) getAlice(path string) *httptest.ResponseRecorder { return e.get("alice", "", path) }

func jobsFixtureNames(t *testing.T) []string {
	var out []string
	for _, full := range contractFixtureNames(t, "jobs") {
		n := strings.TrimPrefix(full, "jobs/")
		read := strings.HasPrefix(n, "jobs_list_") || strings.HasPrefix(n, "job_detail_") || strings.HasPrefix(n, "job_stages_") ||
			strings.HasPrefix(n, "job_leads_") || strings.HasPrefix(n, "job_events_")
		// *_after_* and *_a4_deleted_* were recorded after the write phases; the write routes' tests replay them.
		if !read || strings.Contains(n, "_after") || strings.Contains(n, "_deleted_") {
			continue
		}
		out = append(out, n)
	}
	return out
}

func TestJobsReadMatchesFixtures(t *testing.T) {
	e := newJobsEnv(t, testRedisClient(t))
	e.seedProgress(t)
	replayed := 0
	for _, name := range jobsFixtureNames(t) {
		if name == "job_events_limit_zero" || name == "job_events_limit_negative" {
			continue // Python bug: limit<=0 slices [-0:] / [1:]. Go answers 422 (TestJobEventsLimitClamped)
		}
		fx := contract.Load(t, "jobs/"+name)
		who, ws := jobsActor(name)
		rec := e.get(who, ws, fx.Request.Path)
		got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
		for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
			if v := rec.Header().Get(k); v != "" {
				got.Headers[k] = v
			}
		}
		replayed++
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	if replayed != 50 {
		t.Fatalf("replayed %d read fixtures, want 50", replayed)
	}
}

func jobIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func TestJobsStatusFilterAllowList(t *testing.T) {
	e := newJobsEnv(t, nil)
	cases := map[string]int{
		"": 7, "?status=": 7, "?status=__all__": 7, "?status=done": 2, "?status=running": 2, "?status=failed": 1,
		"?status=DONE": 0, "?status=bogus": 0, "?status=done%27%20OR%201%3D1--": 0, "?status=%25": 0, "?status=__ALL__": 0,
	}
	for q, want := range cases {
		rec := e.getAlice("/api/jobs" + q)
		if rec.Code != 200 {
			t.Fatalf("%q: %d %s", q, rec.Code, rec.Body)
		}
		if got := jobIDs(t, rec); len(got) != want {
			t.Errorf("%q: %d jobs, want %d (%v)", q, len(got), want, got)
		}
	}
	// The empty list is [] (never null).
	if body := e.getAlice("/api/jobs?status=bogus").Body.String(); body != "[]" {
		t.Errorf("empty list body %q", body)
	}
	// Each workspace sees only its own jobs.
	if got := jobIDs(t, e.get("bob", "ws-beta", "/api/jobs")); len(got) != 2 {
		t.Errorf("beta jobs %v", got)
	}
}

func TestJobDetail404ForForeignWorkspace(t *testing.T) {
	e := newJobsEnv(t, nil)
	for _, suffix := range []string{"", "/stages", "/leads", "/events"} {
		rec := e.getAlice("/api/jobs/" + jB1 + suffix)
		if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Job not found"}` {
			t.Errorf("foreign%s: %d %s", suffix, rec.Code, rec.Body)
		}
		rec = e.get("bob", "ws-beta", "/api/jobs/"+jA1+suffix)
		if rec.Code != 404 {
			t.Errorf("reverse foreign%s: %d", suffix, rec.Code)
		}
	}
}

func TestJobLeadsBothAttachmentForms(t *testing.T) {
	e := newJobsEnv(t, nil)
	rec := e.getAlice("/api/jobs/" + jA3 + "/leads")
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 3 {
		t.Fatalf("a3 leads: %d %s %v", rec.Code, rec.Body, err)
	}
	for i, id := range []float64{101, 102, 103} { // score DESC
		if rows[i]["id"] != id {
			t.Errorf("a3 order: %v", rows)
		}
	}
	// Lead.to_dict() key order, not the ledger column order.
	wantPrefix := `[{"id":101,"company":"Mumbai Talent Partners","website":"https://mtp.example.com","email":"hr@mtp.example.com","email_confidence":null,"email_provider":null,"phone":"+91 22 5550 2001","phone_provider":null,"contact_person":null,`
	if !strings.HasPrefix(rec.Body.String(), wantPrefix) {
		t.Errorf("key order: %.400s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"source":"web_search","source_url":null,"collection_job_id":"`+jA3+`","workspace_id":"ws-alpha","score":85,`) {
		t.Errorf("pipeline block order: %s", rec.Body)
	}
	// a4: 104 is attached by source "job:<id>" only, 105 by both forms.
	rec = e.getAlice("/api/jobs/" + jA4 + "/leads")
	rows = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 2 || rows[0]["id"] != float64(104) || rows[1]["id"] != float64(105) {
		t.Fatalf("a4 leads: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `東京ソリューションズ`) {
		t.Errorf("non-ASCII must stay literal: %s", rec.Body)
	}
	if rec := e.getAlice("/api/jobs/" + jA1 + "/leads"); rec.Body.String() != "[]" {
		t.Errorf("job without leads: %s", rec.Body)
	}
}

func TestJobLeadsCrossWorkspaceEmpty(t *testing.T) {
	e := newJobsEnv(t, nil)
	ctx := context.Background()
	// A beta lead naming an alpha job (both forms) and an alpha lead naming a beta job: neither may leak.
	insert := func(ws string, id int, cj, source string) {
		if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO leads (id, workspace_id, company, collection_job_id, source, score) VALUES ($1,$2,'leak',$3,$4,99)`, id, ws, cj, source)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert("ws-beta", 7001, jA3, "x")
	insert("ws-beta", 7002, "", "job:"+jA3)
	insert("ws-alpha", 7003, jB1, "x")
	insert("ws-alpha", 7004, "", "job:"+jB1)
	rec := e.getAlice("/api/jobs/" + jA3 + "/leads")
	if strings.Contains(rec.Body.String(), "leak") {
		t.Fatalf("alpha leads leaked a beta row: %s", rec.Body)
	}
	rec = e.get("bob", "ws-beta", "/api/jobs/"+jB1+"/leads")
	var rows []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0]["company"] != "Beta Staffing Co" {
		t.Fatalf("beta leads: %s", rec.Body)
	}
}

func (e jobsEnv) eventMessages(t *testing.T, path string) []string {
	t.Helper()
	rec := e.getAlice(path)
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Events == nil {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
	}
	var out []string
	for _, ev := range body.Events {
		out = append(out, fmt.Sprint(ev["message"]))
	}
	return out
}

func TestJobEventsFilteredByJobAndMessage(t *testing.T) {
	e := newJobsEnv(t, testRedisClient(t))
	e.seedProgress(t)
	ctx := context.Background()
	key := "leadgen:progress:ws-alpha:recent"
	// Newest first (LPUSH). Falsy messages are dropped like Python's `e.get("message")`; truthy non-strings stay;
	// a non-string job_id never equals the id.
	for _, raw := range []string{
		`{"type":"x","ts":1,"job_id":"` + jA2 + `","message":"","workspace_id":"ws-alpha"}`,
		`{"type":"x","ts":2,"job_id":"` + jA2 + `","message":null,"workspace_id":"ws-alpha"}`,
		`{"type":"x","ts":3,"job_id":"` + jA2 + `","message":0,"workspace_id":"ws-alpha"}`,
		`{"type":"x","ts":4,"job_id":"` + jA2 + `","message":[],"workspace_id":"ws-alpha"}`,
		`{"type":"x","ts":5,"job_id":"` + jA2 + `","message":false,"workspace_id":"ws-alpha"}`,
		`{"type":"x","ts":6,"job_id":"` + jA2 + `","workspace_id":"ws-alpha"}`,
		`{"type":"x","ts":7,"job_id":12,"message":"numeric job id","workspace_id":"ws-alpha"}`,
		`{"type":"x","ts":8,"job_id":"` + jA2 + `","message":7,"workspace_id":"ws-alpha"}`,
	} {
		e.rdb.LPush(ctx, key, raw)
	}
	got := e.eventMessages(t, "/api/jobs/"+jA2+"/events?limit=500")
	want := []string{"Collection queued: IT staffing companies in Bangalore", "Discovering sources", "Found 12 leads — “IT staffing” ✓", "Added Acme HR", "Validating 12 leads", "7"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("events %q\nwant %q", got, want)
	}
	for _, m := range got {
		if m == "Another job's event" || m == "numeric job id" {
			t.Errorf("foreign event leaked: %q", m)
		}
	}
}

func TestJobEventsLimitClamped(t *testing.T) {
	e := newJobsEnv(t, testRedisClient(t))
	e.seedProgress(t)
	base := "/api/jobs/" + jA2 + "/events"
	if got := e.eventMessages(t, base+"?limit=1"); len(got) != 1 || got[0] != "Validating 12 leads" {
		t.Errorf("limit=1 (last event): %q", got)
	}
	if got := e.eventMessages(t, base); len(got) != 5 {
		t.Errorf("default limit: %q", got)
	}
	for _, q := range []string{"?limit=100000", "?limit=99999999999999999999999"} {
		if got := e.eventMessages(t, base+q); len(got) != 5 {
			t.Errorf("%s must clamp to the history, got %q", q, got)
		}
	}
	// Python slices events[-0:] (all) and events[1:] (drops the first) for limit <= 0: Go validates instead.
	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=-99999999999999999999999"} {
		rec := e.getAlice(base + q)
		var body struct {
			Detail []struct {
				Type string `json:"type"`
				Loc  []any  `json:"loc"`
				Msg  string `json:"msg"`
			} `json:"detail"`
		}
		if rec.Code != 422 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Detail) != 1 ||
			body.Detail[0].Type != "greater_than_equal" || body.Detail[0].Msg != "Input should be greater than or equal to 1" {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body)
		}
	}
	// 422 comes before the 404 (FastAPI validates the query before the handler looks the job up).
	if rec := e.getAlice("/api/jobs/nope/events?limit=0"); rec.Code != 422 {
		t.Errorf("422 before 404: %d", rec.Code)
	}
	if rec := e.getAlice(base + "?limit=1.5"); rec.Code != 422 {
		t.Errorf("limit=1.5: %d", rec.Code)
	}
}

func TestJobEventsRedisDownEmpty(t *testing.T) {
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { down.Close() })
	for name, rdb := range map[string]*redis.Client{"nil": nil, "unreachable": down} {
		t.Run(name, func(t *testing.T) { // one env per subtest: each holds the database advisory lock until it ends
			e := newJobsEnv(t, rdb)
			rec := e.getAlice("/api/jobs/" + jA2 + "/events")
			if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"events":[]}` {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
			// the other routes do not depend on Redis
			if rec := e.getAlice("/api/jobs/" + jA2); rec.Code != 200 {
				t.Errorf("detail: %d", rec.Code)
			}
		})
	}
}

func TestJobDetailInvalidDetailsJSON(t *testing.T) {
	e := newJobsEnv(t, nil)
	ctx := context.Background()
	rec := e.getAlice("/api/jobs/" + jA5)
	var d struct {
		IntentDetails json.RawMessage `json:"intent_details"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &d) != nil || string(d.IntentDetails) != "{}" {
		t.Errorf("unparsable intent_details -> {}: %s", rec.Body)
	}
	// The list route returns the stored text untouched.
	if !strings.Contains(e.getAlice("/api/jobs?status=failed").Body.String(), `"intent_details":"not json{"`) {
		t.Error("list must keep the raw intent_details text")
	}
	rec = e.getAlice("/api/jobs/" + jA7 + "/stages")
	if !strings.Contains(rec.Body.String(), `"details":{}`) || strings.Contains(rec.Body.String(), "not-json") {
		t.Errorf("unparsable and empty stage details -> {}: %s", rec.Body)
	}
	// Non-object JSON is returned as parsed; Python-only literals and an exact float repr survive.
	set := func(intent string) {
		if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE collection_jobs SET intent_details=$1 WHERE id=$2`, intent, jA1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for in, want := range map[string]string{`[1,2]`: `[1,2]`, `"s"`: `"s"`, `null`: `null`, `{"a":1.50,"a":2}`: `{"a":2}`} {
		set(in)
		rec := e.getAlice("/api/jobs/" + jA1)
		if !strings.Contains(rec.Body.String(), `"intent_details":`+want+`,`) {
			t.Errorf("intent_details %s: want %s in %s", in, want, rec.Body)
		}
	}
	// NaN/Infinity parse in Python but make the response render fail: a bare 500 (documented, same as Python).
	for _, in := range []string{`{"x": NaN}`, `[Infinity]`, `{"x": -Infinity}`} {
		set(in)
		rec := e.getAlice("/api/jobs/" + jA1)
		if rec.Code != 500 || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" || rec.Body.String() != "Internal Server Error" {
			t.Errorf("%s: %d %q %q", in, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
		// the stages route reads other columns and the list route never parses
		if rec := e.getAlice("/api/jobs/" + jA1 + "/stages"); rec.Code != 200 {
			t.Errorf("stages after NaN intent: %d", rec.Code)
		}
		if rec := e.getAlice("/api/jobs"); rec.Code != 200 {
			t.Errorf("list after NaN intent: %d", rec.Code)
		}
	}
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE collection_job_stages SET details='{"x": NaN}' WHERE job_id=$1`, jA3)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/jobs/" + jA3, "/api/jobs/" + jA3 + "/stages"} {
		if rec := e.getAlice(p); rec.Code != 500 || rec.Body.String() != "Internal Server Error" {
			t.Errorf("%s with NaN stage details: %d %q", p, rec.Code, rec.Body)
		}
	}
}

func TestJobsReadRequiresWorkspaceMembership(t *testing.T) {
	e := newJobsEnv(t, nil)
	for _, p := range []string{"/api/jobs", "/api/jobs/" + jA1, "/api/jobs/" + jA1 + "/stages", "/api/jobs/" + jA1 + "/leads", "/api/jobs/" + jA1 + "/events"} {
		if rec := e.get("", "", p); rec.Code != 401 {
			t.Errorf("%s without token: %d", p, rec.Code)
		}
		if rec := e.get("erin", "ws-beta", p); rec.Code != 403 {
			t.Errorf("%s as non-member: %d", p, rec.Code)
		}
		// reads are open to every member, viewers included (no role gate)
		if rec := e.get("bob", "ws-alpha", p); rec.Code != 200 {
			t.Errorf("%s as plain member: %d", p, rec.Code)
		}
	}
}

// Events are the stored text compacted Python-style: keys keep their stored order (type, ts, ... as
// Python inserted them), not sorted, and non-ASCII is raw like FastAPI's ensure_ascii=False dump.
func TestJobEventsKeepStoredKeyOrder(t *testing.T) {
	e := newJobsEnv(t, testRedisClient(t))
	e.seedProgress(t)
	e.rdb.LPush(context.Background(), "leadgen:progress:ws-alpha:recent",
		`{"type": "zz_order", "ts": 1788000099.0, "message": "café", "job_id": "`+jA2+`", "b": 1, "a": {"y": 1, "x": 2}, "workspace_id": "ws-alpha"}`)
	rec := e.getAlice("/api/jobs/" + jA2 + "/events?limit=1")
	want := `{"events":[{"type":"zz_order","ts":1788000099.0,"message":"café","job_id":"` + jA2 + `","b":1,"a":{"y":1,"x":2},"workspace_id":"ws-alpha"}]}`
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("%d %s\nwant %s", rec.Code, rec.Body, want)
	}
}
