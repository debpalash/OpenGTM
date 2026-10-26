//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Plan 10 Task 15: the audience CRUD routes (list, create, patch, delete) against the recorded Python
// fixtures (testdata/contract/audiences/aud_{list,create,patch,delete}_*) over the plan 10 seed
// (testdata/audiences/seed.json), plus the materialize / schedule / cascade properties.

type audEnv struct {
	mcpEnv
	r *httpx.Router
}

var audSeedTables = []string{"leads", "signals", "audiences", "audience_members", "audience_membership_events", "audience_destinations",
	"destination_runs", "destination_deliveries", "destination_inbound_receipts", "audience_schedules", "destination_inbound_tokens"}

func audOwner(t *testing.T) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func audOwnerInt(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := audOwner(t).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func audPurge(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	c := audOwner(t)
	for i := len(audSeedTables) - 1; i >= 0; i-- {
		if _, err := c.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE workspace_id IN ('ws-alpha','ws-beta')`, audSeedTables[i])); err != nil {
			t.Fatalf("purge %s: %v", audSeedTables[i], err)
		}
	}
	if _, err := c.Exec(ctx, `DELETE FROM jobs WHERE type = 'audience_refresh'`); err != nil {
		t.Fatal(err)
	}
}

func audLoadSeed(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/audiences/seed.json")
	if err != nil {
		t.Fatal(err)
	}
	var seed struct {
		Tables map[string]map[string]struct {
			Columns []string          `json:"columns"`
			Rows    []json.RawMessage `json:"rows"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := audOwner(t)
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		for _, tbl := range audSeedTables {
			td := seed.Tables[ws][tbl]
			if len(td.Rows) == 0 {
				continue
			}
			rows, _ := json.Marshal(td.Rows)
			list := strings.Join(td.Columns, ", ")
			if _, err := c.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (%[2]s) SELECT %[2]s FROM json_populate_recordset(null::%[1]s, $1::json)`, tbl, list), string(rows)); err != nil {
				t.Fatalf("seed %s/%s: %v", ws, tbl, err)
			}
		}
	}
	// The seed inserts explicit serial ids: move every sequence past them so generated ids never collide.
	for _, tbl := range audSeedTables {
		var seq *string
		if err := c.QueryRow(ctx, `SELECT CASE WHEN EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = 'id' AND table_schema = 'public') THEN pg_get_serial_sequence($1, 'id') END`, tbl).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if seq == nil {
			continue
		}
		if _, err := c.Exec(ctx, fmt.Sprintf(`SELECT setval($1, GREATEST(1000, (SELECT coalesce(max(id), 0) FROM %s)))`, tbl), *seq); err != nil {
			t.Fatalf("setval %s: %v", tbl, err)
		}
	}
}

func newAudEnv(t *testing.T, vars ...map[string]string) audEnv {
	t.Helper()
	envVars := map[string]string{}
	for _, m := range vars {
		for k, v := range m {
			envVars[k] = v
		}
	}
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{},
		Getenv: func(k string) string { return envVars[k] },
		Flags: config.Flags{PGLeadStore: true, Audiences: config.AudienceSettings{GoAudienceRoutesEnabled: true, RefreshPageSize: 500,
			GoExecutorAudienceRefresh: envVars["GO_EXECUTOR_AUDIENCE_REFRESH"] == "true"}},
		AudienceEvents: nopEmitter{}}
	RegisterAudiences(r, d)
	e := audEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, r: r}
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "viewer")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	audPurge(t)
	t.Cleanup(func() { audPurge(t) })
	audLoadSeed(t)
	return e
}

func (e audEnv) call(who, ws, method, path, body string) (*httptest.ResponseRecorder, contract.Resp) {
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

func (e audEnv) mustCreate(t *testing.T, body string) map[string]any {
	t.Helper()
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/audiences", body)
	if rec.Code != 201 {
		t.Fatalf("create %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (e audEnv) patch(who, ws, id, body string) (*httptest.ResponseRecorder, map[string]any) {
	rec, _ := e.call(who, ws, "PATCH", "/api/audiences/"+id, body)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec, m
}

var audTimestampPaths = []string{"created_at", "updated_at", "refreshed_at", "next_refresh_at"}

// audFixtureActor is who recorded a fixture and in which workspace (the recorder's headers are redacted).
func audFixtureActor(name string) (user, ws string) {
	switch {
	case strings.Contains(name, "no_auth"):
		return "", ""
	case strings.Contains(name, "erin_editor"), strings.Contains(name, "by_erin"), strings.Contains(name, "nonmember"):
		if strings.Contains(name, "nonmember") {
			return "erin", "ws-beta"
		}
		return "erin", "ws-alpha"
	case strings.Contains(name, "viewer"):
		return "bob", "ws-alpha"
	case strings.Contains(name, "beta_bob"), strings.Contains(name, "same_name_other_ws"):
		return "bob", "ws-beta"
	}
	return "alice", "ws-alpha"
}

var audIDRe = regexp.MustCompile(`00000000-0000-4000-8000-\d{12}`)

// audReplay runs one recorded request and compares it. ids maps recorded created ids to the Go ones.
func (e audEnv) replay(t *testing.T, name string, ids map[string]string, opts ...contract.Option) contract.Resp {
	t.Helper()
	fx := contract.Load(t, "audiences/"+name)
	user, ws := audFixtureActor(name)
	path := audIDRe.ReplaceAllStringFunc(fx.Request.Path, func(s string) string {
		if v, ok := ids[s]; ok {
			return v
		}
		return s
	})
	body := ""
	if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
		body = string(fx.Request.Body)
	}
	_, got := e.call(user, ws, fx.Request.Method, path, body)
	if fx.Response.Status/100 == 2 && len(fx.Response.Body) > 0 && fx.Response.Body[0] == '{' {
		opts = append(opts, contract.IgnorePaths("$.id"))
		for _, k := range audTimestampPaths {
			opts = append(opts, contract.IgnoreShapeOf("$."+k))
		}
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
	return got
}

func audFixtureNames(t *testing.T, prefix string) []string {
	t.Helper()
	ents, err := os.ReadDir("../../testdata/contract/audiences")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ent := range ents {
		if n := strings.TrimSuffix(ent.Name(), ".json"); strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func TestAudienceCRUDMatchesFixtures(t *testing.T) {
	e := newAudEnv(t)
	ids := map[string]string{}

	// Phase 0: reads over the seed alone (exact, timestamps included).
	for _, n := range []string{"aud_list_default", "aud_list_beta_bob", "aud_list_erin_editor", "aud_list_viewer_bob_alpha", "aud_list_nonmember", "aud_list_no_auth"} {
		e.replay(t, n, ids)
	}

	// Phase 1: creates that succeeded, in recorded id order (so updated_at order matches the recording).
	type created struct{ name, recID string }
	var creates []created
	var errCreates []string
	for _, n := range audFixtureNames(t, "aud_create_") {
		fx := contract.Load(t, "audiences/"+n)
		if fx.Response.Status != 201 || n == "aud_create_same_name_other_ws" {
			errCreates = append(errCreates, n)
			continue
		}
		var b struct{ ID string }
		_ = json.Unmarshal(fx.Response.Body, &b)
		creates = append(creates, created{n, b.ID})
	}
	sort.Slice(creates, func(i, j int) bool { return creates[i].recID < creates[j].recID })
	for _, c := range creates {
		got := e.replay(t, c.name, ids)
		var b struct{ ID string }
		_ = json.Unmarshal(got.Body, &b)
		ids[c.recID] = b.ID
	}
	for _, n := range errCreates {
		e.replay(t, n, ids)
	}

	// Phase 2: patches that succeeded, in the recorded updated_at order, then the error ones.
	for _, n := range []string{"aud_patch_name", "aud_patch_description", "aud_patch_empty_body", "aud_patch_filters_rematerialize",
		"aud_patch_filters_to_empty", "aud_patch_interval_reschedules", "aud_patch_refresh_disabled", "aud_patch_refresh_reenabled",
		"aud_patch_filters_and_name", "aud_patch_same_name_ok"} {
		e.replay(t, n, ids)
	}
	done := map[string]bool{"aud_patch_seeded_name_after": true, "aud_patch_filters_null": true}
	for _, n := range []string{"aud_patch_name", "aud_patch_description", "aud_patch_empty_body", "aud_patch_filters_rematerialize",
		"aud_patch_filters_to_empty", "aud_patch_interval_reschedules", "aud_patch_refresh_disabled", "aud_patch_refresh_reenabled",
		"aud_patch_filters_and_name", "aud_patch_same_name_ok"} {
		done[n] = true
	}
	for _, n := range audFixtureNames(t, "aud_patch_") {
		if !done[n] {
			e.replay(t, n, ids)
		}
	}
	e.replay(t, "aud_patch_filters_null", ids)

	// Phase 3: the list after the writes (seed rows exact, created rows by position; recorded order is updated_at DESC).
	fx := contract.Load(t, "audiences/aud_patch_seeded_name_after")
	var want []map[string]any
	_ = json.Unmarshal(fx.Response.Body, &want)
	var opts []contract.Option
	for i, a := range want {
		if id, _ := a["id"].(string); !strings.HasPrefix(id, "aud-") {
			opts = append(opts, contract.IgnorePaths(fmt.Sprintf("$[%d].id", i)))
			for _, k := range audTimestampPaths {
				opts = append(opts, contract.IgnoreShapeOf(fmt.Sprintf("$[%d].%s", i, k)))
			}
		}
	}
	e.replay(t, "aud_patch_seeded_name_after", ids, opts...)

	// Phase 4: deletes (the seeded audience with destinations goes last).
	e.replay(t, "aud_delete_created", ids)
	e.replay(t, "aud_delete_again_404", ids)
	for _, n := range audFixtureNames(t, "aud_delete_") {
		if n != "aud_delete_with_destinations" && n != "aud_delete_created" && n != "aud_delete_again_404" {
			e.replay(t, n, ids)
		}
	}
	e.replay(t, "aud_delete_with_destinations", ids)
}

func TestAudienceResponseKeyOrder(t *testing.T) {
	e := newAudEnv(t)
	var shapes struct {
		Shapes map[string][]string `json:"shapes"`
	}
	raw, err := os.ReadFile("../../testdata/contract/audiences/response_keys.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &shapes); err != nil {
		t.Fatal(err)
	}
	keys := func(body []byte) []string {
		dec := json.NewDecoder(bytes.NewReader(body))
		_, _ = dec.Token()
		var out []string
		for dec.More() {
			k, _ := dec.Token()
			out = append(out, k.(string))
			var skip json.RawMessage
			_ = dec.Decode(&skip)
		}
		return out
	}
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/audiences", `{"name":"Order","description":"<b>&</b>"}`)
	if got := keys(rec.Body.Bytes()); strings.Join(got, ",") != strings.Join(shapes.Shapes["audience"], ",") {
		t.Errorf("create key order %v", got)
	}
	if !strings.Contains(rec.Body.String(), `"description":"<b>&</b>"`) {
		t.Errorf("HTML must not be escaped: %s", rec.Body.String())
	}
	var list []json.RawMessage
	rec, _ = e.call("alice", "ws-alpha", "GET", "/api/audiences", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) == 0 {
		t.Fatal(err, rec.Body.String())
	}
	if got := keys(list[0]); strings.Join(got, ",") != strings.Join(shapes.Shapes["audience"], ",") {
		t.Errorf("list key order %v", got)
	}
	rec, _ = e.patch("alice", "ws-alpha", "aud-act-1", `{"description":"x"}`)
	if got := keys(rec.Body.Bytes()); strings.Join(got, ",") != strings.Join(shapes.Shapes["audience"], ",") {
		t.Errorf("patch key order %v", got)
	}
}

func (e audEnv) pendingJobs(t *testing.T, audienceID string) (n int, executor string, nextRun time.Time) {
	t.Helper()
	ctx := context.Background()
	c := audOwner(t)
	rows, err := c.Query(ctx, `SELECT executor, next_run_at FROM jobs WHERE type = 'audience_refresh' AND status = 'pending'
		AND fire_key LIKE 'audience_refresh:' || $1 || ':%'`, audienceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		n++
		if err := rows.Scan(&executor, &nextRun); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func audCount(t *testing.T, table, id string) int {
	t.Helper()
	return audOwnerInt(t, fmt.Sprintf(`SELECT count(*) FROM %s WHERE audience_id = $1`, table), id)
}

func TestCreateMaterializesAndSchedules(t *testing.T) {
	e := newAudEnv(t)
	a := e.mustCreate(t, `{"name":"Mat","filters":{"city":"Austin"}}`)
	id := a["id"].(string)
	if a["member_count"] != float64(3) || a["refresh_health"] != "healthy" || a["refreshed_at"] == nil || a["next_refresh_at"] == nil {
		t.Fatalf("response must reflect the refreshed + scheduled row: %v", a)
	}
	if n := audCount(t, "audience_members", id); n != 3 {
		t.Errorf("members %d", n)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM audience_membership_events WHERE audience_id = $1 AND event_type = 'entered'`, id); n != 3 {
		t.Errorf("entered events %d", n)
	}
	n, exec, next := e.pendingJobs(t, id)
	if n != 1 || exec != "python" {
		t.Fatalf("pending refresh jobs %d executor %q", n, exec)
	}
	if d := time.Until(next.UTC()); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("next run in %v", d)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM audience_schedules WHERE audience_id = $1 AND enabled AND next_refresh_at IS NOT NULL`, id); n != 1 {
		t.Errorf("schedule mirror rows %d", n)
	}

	b := e.mustCreate(t, `{"name":"Off","refresh_enabled":false}`)
	bid := b["id"].(string)
	if n, _, _ := e.pendingJobs(t, bid); n != 0 || b["next_refresh_at"] != nil {
		t.Errorf("disabled audience scheduled: jobs %d next %v", n, b["next_refresh_at"])
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM audience_schedules WHERE audience_id = $1 AND NOT enabled`, bid); n != 1 {
		t.Errorf("disabled mirror rows %d", n)
	}
	if b["member_count"] != float64(15) {
		t.Errorf("disabled audiences still materialize: %v", b["member_count"])
	}
}

func TestCreateDuplicateName409(t *testing.T) {
	e := newAudEnv(t)
	before := audOwnerInt(t, `SELECT count(*) FROM audiences WHERE workspace_id = 'ws-alpha'`)
	for _, body := range []string{`{"name":"Everyone"}`, `{"name":"  Everyone "}`} {
		rec, got := e.call("alice", "ws-alpha", "POST", "/api/audiences", body)
		if rec.Code != 409 || string(got.Body) != `{"detail":"An audience with this name already exists"}` {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM audiences WHERE workspace_id = 'ws-alpha'`); n != before {
		t.Errorf("audiences %d -> %d", before, n)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM jobs WHERE type = 'audience_refresh'`); n != 0 {
		t.Errorf("a refused create scheduled %d jobs", n)
	}
	if rec, _ := e.call("bob", "ws-beta", "POST", "/api/audiences", `{"name":"Everyone"}`); rec.Code != 201 {
		t.Errorf("same name in another workspace: %d", rec.Code)
	}
}

func TestPatchFiltersRematerializes(t *testing.T) {
	e := newAudEnv(t)
	a := e.mustCreate(t, `{"name":"Re"}`)
	id := a["id"].(string)
	if a["member_count"] != float64(15) {
		t.Fatalf("seed leads: %v", a["member_count"])
	}
	rec, m := e.patch("alice", "ws-alpha", id, `{"filters":{"city":"Austin"}}`)
	if rec.Code != 200 || m["member_count"] != float64(3) {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	if n := audCount(t, "audience_members", id); n != 3 {
		t.Errorf("members after narrowing %d", n)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM audience_membership_events WHERE audience_id = $1 AND event_type = 'exited'`, id); n != 12 {
		t.Errorf("exited events %d", n)
	}
	// Equal filters still re-materialize (Python tests "filters" in changes, not inequality).
	before := audOwnerInt(t, `SELECT count(*) FROM audience_membership_events WHERE audience_id = $1`, id)
	var r1, r2 string
	_ = audOwner(t).QueryRow(context.Background(), `SELECT refreshed_at::text FROM audiences WHERE id = $1`, id).Scan(&r1)
	time.Sleep(5 * time.Millisecond)
	if rec, _ := e.patch("alice", "ws-alpha", id, `{"filters":{"city":"Austin"}}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	_ = audOwner(t).QueryRow(context.Background(), `SELECT refreshed_at::text FROM audiences WHERE id = $1`, id).Scan(&r2)
	if r1 == r2 {
		t.Error("an equal filters patch must still refresh")
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM audience_membership_events WHERE audience_id = $1`, id); n != before {
		t.Errorf("unchanged membership emitted events: %d -> %d", before, n)
	}
	// filters null stores JSON null, reads back {} and materializes everyone.
	rec, m = e.patch("alice", "ws-alpha", id, `{"filters":null}`)
	if rec.Code != 200 || m["member_count"] != float64(15) || fmt.Sprint(m["filters"]) != "map[]" {
		t.Errorf("filters null: %d %s", rec.Code, rec.Body.String())
	}
	// An invalid patch changes nothing.
	if rec, _ := e.patch("alice", "ws-alpha", id, `{"filters":{"min_score":9,"max_score":1}}`); rec.Code != 422 {
		t.Errorf("min>max: %d", rec.Code)
	}
}

func TestPatchIntervalReschedules(t *testing.T) {
	e := newAudEnv(t)
	a := e.mustCreate(t, `{"name":"Sched"}`)
	id := a["id"].(string)
	_, _, first := e.pendingJobs(t, id)
	rec, m := e.patch("alice", "ws-alpha", id, `{"refresh_interval_minutes":30}`)
	if rec.Code != 200 || m["refresh_interval_minutes"] != float64(30) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	n, _, next := e.pendingJobs(t, id)
	if n != 1 {
		t.Fatalf("exactly one pending job after reschedule, got %d", n)
	}
	if !next.Before(first) || time.Until(next.UTC()) > 31*time.Minute {
		t.Errorf("job not moved earlier: %v -> %v", first, next)
	}
	if c := audOwnerInt(t, `SELECT count(*) FROM jobs WHERE type='audience_refresh' AND status='cancelled' AND fire_key LIKE 'audience_refresh:'||$1||':%'`, id); c != 1 {
		t.Errorf("cancelled predecessors %d", c)
	}
	// Disabling clears the schedule; re-enabling restores it.
	if rec, m := e.patch("alice", "ws-alpha", id, `{"refresh_enabled":false}`); rec.Code != 200 || m["next_refresh_at"] != nil {
		t.Fatalf("disable: %s", rec.Body.String())
	}
	if n, _, _ := e.pendingJobs(t, id); n != 0 {
		t.Errorf("disabled but %d pending jobs", n)
	}
	if rec, m := e.patch("alice", "ws-alpha", id, `{"refresh_enabled":true,"refresh_interval_minutes":15}`); rec.Code != 200 || m["next_refresh_at"] == nil {
		t.Fatalf("enable: %s", rec.Body.String())
	}
	if n, _, nx := e.pendingJobs(t, id); n != 1 || time.Until(nx.UTC()) > 16*time.Minute {
		t.Errorf("re-enabled: jobs %d next %v", n, nx)
	}
}

func TestPatchOnlyGivenFields(t *testing.T) {
	e := newAudEnv(t)
	a := e.mustCreate(t, `{"name":"Given","description":"d0","filters":{"city":"Austin"},"refresh_interval_minutes":45}`)
	id := a["id"].(string)
	_, _, jobRun := e.pendingJobs(t, id)
	var refreshed, updated string
	snap := func() {
		_ = audOwner(t).QueryRow(context.Background(), `SELECT refreshed_at::text, updated_at::text FROM audiences WHERE id = $1`, id).Scan(&refreshed, &updated)
	}
	snap()
	rec, m := e.patch("alice", "ws-alpha", id, `{"description":"d1"}`)
	if rec.Code != 200 || m["description"] != "d1" || m["name"] != "Given" || m["refresh_interval_minutes"] != float64(45) ||
		fmt.Sprint(m["filters"]) != "map[city:Austin]" || m["member_count"] != float64(3) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var refreshed2, updated2 string
	_ = audOwner(t).QueryRow(context.Background(), `SELECT refreshed_at::text, updated_at::text FROM audiences WHERE id = $1`, id).Scan(&refreshed2, &updated2)
	if refreshed2 != refreshed {
		t.Error("a description patch must not re-materialize")
	}
	if updated2 == updated {
		t.Error("a real change bumps updated_at")
	}
	if _, _, again := e.pendingJobs(t, id); !again.Equal(jobRun) {
		t.Error("a description patch must not reschedule")
	}
	// Nothing changed -> no UPDATE -> updated_at stays (SQLAlchemy flushes changed attributes only).
	updated = updated2
	for _, body := range []string{`{}`, `{"name":"Given"}`, `{"name":"  Given  ","description":"d1"}`} {
		if rec, _ := e.patch("alice", "ws-alpha", id, body); rec.Code != 200 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
		snap()
		if updated != updated2 && refreshed == refreshed2 {
			t.Errorf("%s bumped updated_at", body)
		}
	}
	// null on a NOT NULL column is the 409 the recording shows; nothing is applied.
	for _, body := range []string{`{"name":null}`, `{"description":null}`, `{"refresh_enabled":null}`, `{"refresh_interval_minutes":null}`, `{"description":"zz","name":null}`} {
		rec, _ := e.patch("alice", "ws-alpha", id, body)
		if rec.Code != 409 || rec.Body.String() != `{"detail":"An audience with this name already exists"}` {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if _, m := e.patch("alice", "ws-alpha", id, `{}`); m["description"] != "d1" {
		t.Errorf("a 409 patch leaked: %v", m["description"])
	}
	if rec, _ := e.patch("alice", "ws-alpha", id, `{"name":"Everyone"}`); rec.Code != 409 {
		t.Errorf("duplicate rename: %d", rec.Code)
	}
}

func TestDeleteCascadesAndUnschedules(t *testing.T) {
	e := newAudEnv(t)
	a := e.mustCreate(t, `{"name":"Doomed"}`)
	id := a["id"].(string)
	rec, got := e.call("alice", "ws-alpha", "DELETE", "/api/audiences/"+id, "")
	if rec.Code != 204 || rec.Body.Len() != 0 || got.Headers["Content-Type"] != "application/json" {
		t.Fatalf("%d %q %v", rec.Code, rec.Body.String(), got.Headers)
	}
	for _, tbl := range []string{"audience_members", "audience_membership_events", "audience_schedules"} {
		if n := audCount(t, tbl, id); n != 0 {
			t.Errorf("%s rows left: %d", tbl, n)
		}
	}
	if n, _, _ := e.pendingJobs(t, id); n != 0 {
		t.Errorf("pending jobs left: %d", n)
	}
	if audOwnerInt(t, `SELECT count(*) FROM jobs WHERE type='audience_refresh' AND status='cancelled' AND fire_key LIKE 'audience_refresh:'||$1||':%'`, id) != 1 {
		t.Error("the pending refresh job must be cancelled")
	}
	if rec, _ := e.call("alice", "ws-alpha", "DELETE", "/api/audiences/"+id, ""); rec.Code != 404 {
		t.Errorf("second delete: %d", rec.Code)
	}
	// seeded audience with members, events and destinations
	for _, tbl := range []string{"audience_members", "audience_membership_events", "audience_destinations"} {
		if audCount(t, tbl, "aud-act-1") == 0 {
			t.Fatalf("seed has no %s for aud-act-1", tbl)
		}
	}
	if rec, _ := e.call("alice", "ws-alpha", "DELETE", "/api/audiences/aud-act-1", ""); rec.Code != 204 {
		t.Fatalf("delete seeded: %d", rec.Code)
	}
	for _, tbl := range []string{"audience_members", "audience_membership_events", "audience_destinations", "audience_schedules"} {
		if n := audCount(t, tbl, "aud-act-1"); n != 0 {
			t.Errorf("%s rows left for the seeded audience: %d", tbl, n)
		}
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM destination_runs WHERE workspace_id='ws-alpha' AND destination_id NOT IN (SELECT id FROM audience_destinations)`); n != 0 {
		t.Errorf("orphaned destination runs: %d", n)
	}
}

func TestValidation422Shapes(t *testing.T) {
	e := newAudEnv(t)
	for _, c := range []struct{ name, body, typ, loc, msg string }{
		{"empty body", ``, "missing", "", "Field required"},
		{"array", `[]`, "model_attributes_type", "", "Input should be a valid dictionary or object to extract fields from"},
		{"bad json", `{"name":`, "json_invalid", "8", "JSON decode error"},
		{"min_score float", `{"name":"x","filters":{"min_score":5.0}}`, "value_error", "filters", "Value error, min_score must be an integer"},
		{"interval text", `{"name":"x","refresh_interval_minutes":"abc"}`, "int_parsing", "refresh_interval_minutes", "Input should be a valid integer, unable to parse string as an integer"},
		{"interval float", `{"name":"x","refresh_interval_minutes":30.5}`, "int_from_float", "refresh_interval_minutes", "Input should be a valid integer, got a number with a fractional part"},
		{"interval 1e400", `{"name":"x","refresh_interval_minutes":1e400}`, "finite_number", "refresh_interval_minutes", "Input should be a finite number"},
		{"interval string float", `{"name":"x","refresh_interval_minutes":"20.5"}`, "int_parsing", "refresh_interval_minutes", "Input should be a valid integer, unable to parse string as an integer"},
		{"interval double underscore", `{"name":"x","refresh_interval_minutes":"2__0"}`, "int_parsing", "refresh_interval_minutes", "Input should be a valid integer, unable to parse string as an integer"},
		{"interval too big", `{"name":"x","refresh_interval_minutes":10081}`, "less_than_equal", "refresh_interval_minutes", "Input should be less than or equal to 10080"},
		{"description null", `{"name":"x","description":null}`, "string_type", "description", "Input should be a valid string"},
		{"filters null", `{"name":"x","filters":null}`, "dict_type", "filters", "Input should be a valid dictionary"},
		{"refresh_enabled null", `{"name":"x","refresh_enabled":null}`, "bool_type", "refresh_enabled", "Input should be a valid boolean"},
	} {
		rec, _ := e.call("alice", "ws-alpha", "POST", "/api/audiences", c.body)
		var b struct {
			Detail []struct {
				Type string
				Loc  []any
				Msg  string
			}
		}
		if rec.Code != 422 || json.Unmarshal(rec.Body.Bytes(), &b) != nil || len(b.Detail) != 1 {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
			continue
		}
		d := b.Detail[0]
		loc := ""
		if len(d.Loc) > 1 {
			loc = fmt.Sprint(d.Loc[1])
		}
		if d.Type != c.typ || loc != c.loc || d.Msg != c.msg || d.Loc[0] != "body" {
			t.Errorf("%s: %s", c.name, rec.Body.String())
		}
	}
	// every field is validated and the errors come in model field order
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/audiences", `{"name":5,"description":6,"filters":[],"refresh_enabled":"maybe","refresh_interval_minutes":1}`)
	var multi struct{ Detail []struct{ Loc []any } }
	_ = json.Unmarshal(rec.Body.Bytes(), &multi)
	var order []string
	for _, d := range multi.Detail {
		order = append(order, fmt.Sprint(d.Loc[1]))
	}
	if rec.Code != 422 || strings.Join(order, ",") != "name,description,filters,refresh_enabled,refresh_interval_minutes" {
		t.Errorf("multi-error order %v: %s", order, rec.Body.String())
	}
	// valid integers survive the real decoder (UseNumber): {"min_score":5} is accepted and read back as 5
	m := e.mustCreate(t, `{"name":"Num","filters":{"min_score":5,"max_score":100000000000000000000000}}`)
	if f, _ := m["filters"].(map[string]any); f["min_score"] != float64(5) {
		t.Errorf("filters %v", m["filters"])
	}
	if !strings.Contains(fmt.Sprintf("%v", m["filters"]), "min_score:5") {
		t.Errorf("filters %v", m["filters"])
	}
	// lax ints and the order of duplicate keys (last wins)
	m = e.mustCreate(t, `{"name":"Lax","refresh_interval_minutes":"30","name":"Lax2","description":"a","description":"b"}`)
	if m["name"] != "Lax2" || m["description"] != "b" || m["refresh_interval_minutes"] != float64(30) {
		t.Errorf("%v", m)
	}
	for i, v := range []string{`"20.0"`, `"2_0"`, `" +20.00 "`, `20.0`} {
		if m := e.mustCreate(t, fmt.Sprintf(`{"name":"Int%d","refresh_interval_minutes":%s}`, i, v)); m["refresh_interval_minutes"] != float64(20) {
			t.Errorf("%s: %v", v, m["refresh_interval_minutes"])
		}
	}
	m = e.mustCreate(t, fmt.Sprintf(`{"name":%q}`, strings.Repeat("é", 200)))
	if m["name"] != strings.Repeat("é", 200) {
		t.Error("a 200 character name counts characters, not bytes")
	}
	// body cap
	rec, _ = e.call("alice", "ws-alpha", "POST", "/api/audiences", `{"name":"big","description":"`+strings.Repeat("x", 2<<20)+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d", rec.Code)
	}
	// authentication, then workspace role, then validation
	if rec, _ := e.call("", "", "POST", "/api/audiences", `{}`); rec.Code != 401 {
		t.Errorf("anon: %d", rec.Code)
	}
	if rec, _ := e.call("bob", "ws-alpha", "POST", "/api/audiences", `{}`); rec.Code != 403 {
		t.Errorf("viewer with an invalid body must get 403: %d", rec.Code)
	}
	// 404 comes after body validation
	if rec, _ := e.patch("alice", "ws-alpha", "nope", `{"name":""}`); rec.Code != 422 {
		t.Errorf("validation precedes lookup: %d", rec.Code)
	}
	if rec, _ := e.patch("alice", "ws-alpha", "nope%00", `{"name":"x"}`); rec.Code != 404 {
		t.Errorf("NUL id: %d", rec.Code)
	}
}

func TestEditorRequiredViewerReadsOnly(t *testing.T) {
	e := newAudEnv(t)
	if rec, _ := e.call("bob", "ws-alpha", "GET", "/api/audiences", ""); rec.Code != 200 {
		t.Errorf("viewer list: %d", rec.Code)
	}
	before := audOwnerInt(t, `SELECT count(*) FROM audiences WHERE workspace_id='ws-alpha'`)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/audiences", `{"name":"v"}`},
		{"PATCH", "/api/audiences/aud-act-1", `{"name":"v"}`},
		{"DELETE", "/api/audiences/aud-act-1", ""},
	} {
		rec, _ := e.call("bob", "ws-alpha", c.method, c.path, c.body)
		if rec.Code != 403 || rec.Body.String() != `{"detail":"Workspace permission denied: audiences.write"}` {
			t.Errorf("viewer %s: %d %s", c.method, rec.Code, rec.Body.String())
		}
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM audiences WHERE workspace_id='ws-alpha' AND name='v'`); n != 0 || audOwnerInt(t, `SELECT count(*) FROM audiences WHERE workspace_id='ws-alpha'`) != before {
		t.Error("a forbidden write changed data")
	}
	if rec, _ := e.call("erin", "ws-alpha", "POST", "/api/audiences", `{"name":"editor ok"}`); rec.Code != 201 {
		t.Errorf("editor create: %d", rec.Code)
	}
	if rec, _ := e.call("erin", "ws-beta", "GET", "/api/audiences", ""); rec.Code != 403 {
		t.Errorf("non-member: %d", rec.Code)
	}
}

func TestAudienceForeignWorkspace404(t *testing.T) {
	e := newAudEnv(t)
	for _, c := range []struct{ method, body string }{{"PATCH", `{"name":"x"}`}, {"DELETE", ""}} {
		rec, _ := e.call("alice", "ws-alpha", c.method, "/api/audiences/aud-beta-act", c.body)
		if rec.Code != 404 || rec.Body.String() != `{"detail":"Audience not found"}` {
			t.Errorf("%s foreign: %d %s", c.method, rec.Code, rec.Body.String())
		}
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM audiences WHERE id='aud-beta-act' AND name <> 'x'`); n != 1 {
		t.Error("the foreign audience was modified or deleted")
	}
	if rec, _ := e.call("bob", "ws-beta", "DELETE", "/api/audiences/aud-act-1", ""); rec.Code != 404 {
		t.Errorf("beta owner deleting an alpha audience: %d", rec.Code)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM audiences WHERE id='aud-act-1'`); n != 1 {
		t.Error("alpha audience deleted from beta")
	}
	rec, _ := e.call("bob", "ws-beta", "GET", "/api/audiences", "")
	if strings.Contains(rec.Body.String(), "aud-act") {
		t.Errorf("beta list leaks alpha: %s", rec.Body.String())
	}
}

func TestAudienceCRUDRoutePrecedence(t *testing.T) {
	e := newAudEnv(t)
	hit := ""
	e.r.Handle("DELETE /api/audiences/literal-probe", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = "literal"
		w.WriteHeader(204)
	}))
	h := e.r.Handler()
	do := func(method, path string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		for k, v := range bearer("alice") {
			req.Header.Set(k, v)
		}
		req.Header.Set("X-Workspace-Id", "ws-alpha")
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("DELETE", "/api/audiences/literal-probe"); c != 204 || hit != "literal" {
		t.Errorf("literal route must win over {audience_id}: %d %q", c, hit)
	}
	if c := do("PATCH", "/api/audiences/literal-probe"); c != 404 {
		t.Errorf("other methods fall through to {audience_id}: %d", c)
	}
}

func TestListOrderTiebreakAndCap(t *testing.T) {
	e := newAudEnv(t)
	if _, err := audOwner(t).Exec(context.Background(), `UPDATE audiences SET updated_at = '2030-01-01 00:00:00' WHERE id IN ('aud-act-3','aud-act-1','aud-alpha-1')`); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.call("alice", "ws-alpha", "GET", "/api/audiences", "")
	var list []struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	var got []string
	for _, a := range list {
		got = append(got, a.ID)
	}
	if strings.Join(got, ",") != "aud-act-1,aud-act-3,aud-alpha-1,aud-act-2" {
		t.Errorf("order %v", got)
	}
	old := audCrudListCap
	audCrudListCap = 2
	defer func() { audCrudListCap = old }()
	rec, _ = e.call("alice", "ws-alpha", "GET", "/api/audiences", "")
	list = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 2 {
		t.Errorf("cap: %d rows", len(list))
	}
}

// The refresh job's executor follows go_executor.audience_refresh_executor: enabled + cohort workspace + flag.
func TestCreateExecutorCohort(t *testing.T) {
	cases := []struct {
		name string
		vars map[string]string
		want string
	}{
		{"all on", map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "ws-beta, ws-alpha", "GO_EXECUTOR_AUDIENCE_REFRESH": "true"}, "go"},
		{"not in cohort", map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "ws-beta", "GO_EXECUTOR_AUDIENCE_REFRESH": "true"}, "python"},
		{"kind flag off", map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "ws-alpha"}, "python"},
		{"executor off", map[string]string{"GO_EXECUTOR_WORKSPACES": "ws-alpha", "GO_EXECUTOR_AUDIENCE_REFRESH": "true"}, "python"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newAudEnv(t, c.vars)
			a := e.mustCreate(t, `{"name":"Cohort"}`)
			if n, exec, _ := e.pendingJobs(t, a["id"].(string)); n != 1 || exec != c.want {
				t.Errorf("jobs %d executor %q, want %q", n, exec, c.want)
			}
		})
	}
}
