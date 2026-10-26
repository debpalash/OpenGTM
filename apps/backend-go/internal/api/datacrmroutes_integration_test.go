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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// dataFixtureOrder is the recording order of the datacrm fixtures (the case file order):
// the mutating cases change the rows later cases read, so replay must keep it.
var dataFixtureOrder = []string{
	"list_default", "list_no_auth", "list_non_admin_bob", "list_non_admin_erin",
	"list_limit_5_offset_5", "list_limit_0", "list_offset_beyond_total", "list_limit_negative",
	"list_offset_negative", "list_limit_not_int", "list_q_name_accent", "list_q_email_case_insensitive",
	"list_q_percent_wildcard", "list_q_no_match", "list_source_link_id", "list_source_link_id_zero_ignored",
	"list_start_date", "list_end_date", "list_date_range", "list_is_used_true",
	"list_is_used_false", "list_is_used_not_bool", "list_sort_id_asc", "list_sort_email_asc",
	"list_sort_email_desc", "list_sort_created_at_asc", "list_sort_created_at_desc", "list_sort_unknown_falls_back_to_id",
	"list_sort_metadata_crash", "list_order_unknown_is_desc", "list_combined_filters", "stats_timeline",
	"stats_timeline_non_admin", "stats_domains_default", "stats_domains_limit_2", "stats_domains_limit_0",
	"stats_domains_limit_negative", "stats_domains_limit_not_int", "stats_domains_non_admin", "export_all",
	"export_filtered", "export_empty", "export_sort_metadata_crash", "export_non_admin",
	"export_no_auth", "mark_used_non_admin", "mark_used_no_auth", "mark_used_bad_body",
	"mark_used_ids", "list_after_mark_ids", "mark_used_ids_unused", "mark_used_filter_q",
	"mark_used_filter_source_link", "mark_used_filter_dates_and_used", "mark_used_filter_no_match", "list_after_mark_filters",
	"mark_used_empty_marks_every_row", "mark_used_empty_unmarks_every_row", "patch_non_admin", "patch_no_auth",
	"patch_ok", "patch_partial_email_only", "patch_null_fields", "patch_empty_body",
	"patch_unknown_field_ignored", "patch_not_found", "patch_bad_type", "patch_id_not_int",
	"delete_non_admin", "delete_no_auth", "delete_ok", "delete_again_not_found",
	"delete_id_not_int", "list_after_edits", "list_total_after_delete",
}

type dataEnv struct {
	env
	r     *httpx.Router
	owner *pgxpool.Pool
}

func newDataEnv(t *testing.T) dataEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	RegisterDataCRM(r, Deps{Pool: pool, Signer: testSigner, Identity: st,
		Flags: config.Flags{PGLeadStore: true, GoConnectorRoutesEnabled: true}})
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	e := dataEnv{env: env{h: r.Handler(), st: st}, r: r, owner: owner}
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "editor", false)
	e.seed(t, "wsadmin", pythonHash, true, "user", false)
	e.seedData(t)
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `TRUNCATE email_data, links RESTART IDENTITY CASCADE`) })
	return e
}

// seedData ports legacy/python/scripts/contract_seed.py::_seed_connectors (links and
// email_data only) so the recorded fixtures replay byte for byte.
func (e dataEnv) seedData(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.owner.Exec(ctx, `TRUNCATE email_data, links RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	iso := func(tm time.Time) string {
		s := tm.UTC().Format("2006-01-02T15:04:05")
		if us := tm.Nanosecond() / 1000; us != 0 {
			s += fmt.Sprintf(".%06d", us)
		}
		return s + "+00:00"
	}
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	statuses := []string{"Pending", "Processing", "Completed", "Failed"}
	for n := 1; n <= 30; n++ {
		created := iso(base.Add(-time.Duration((n*3)%45)*24*time.Hour - time.Duration(n%24)*time.Hour))
		var src, upd any
		if n%6 != 0 {
			src = "seed"
		}
		if n%2 == 0 {
			upd = created
		}
		if _, err := e.owner.Exec(ctx, `INSERT INTO links (id, url, status, created_at, source, updated_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			n, fmt.Sprintf("https://source%02d.example.com/list", n), statuses[n%4], created, src, upd); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"José Álvarez", "Zoë Müller", "Łukasz Nowak", "Renée O'Brien", "李雷", "Åsa Lindström", "Chloé Dupont",
		"Ünal Şahin", "Sam \"Sammy\" Lee", "Ana Pérez"}
	domains := []string{"Example.COM", "ACME.io", "Café.Example.fr", "mail.Example.com", "ExAmPlE.org", "acme.io"}
	for i := 1; i <= 60; i++ {
		days := (i*3 + i%5) % 45
		created := base.Add(-time.Duration(days)*24*time.Hour - time.Duration((i*7)%24)*time.Hour - time.Duration((i*11)%60)*time.Minute)
		if i%4 == 0 {
			created = created.Add(time.Duration(i*1111) * time.Microsecond)
		}
		var name, email, createdAt, link, tags, notes any
		switch {
		case i%9 == 0:
		case i == 5:
			name = ""
		default:
			name = names[i%len(names)]
		}
		email = fmt.Sprintf("User%02d@%s", i, domains[i%len(domains)])
		if i == 13 {
			email = "not-an-email"
		}
		if i == 21 {
			email = nil
		}
		if i != 34 {
			createdAt = iso(created)
		}
		switch {
		case i%11 == 0:
		case i == 44:
			link = 999
		default:
			link = (i-1)%30 + 1
		}
		if i%5 != 0 {
			if i%2 == 1 {
				tags = "prospect,vip"
			} else {
				tags = "cold"
			}
		}
		if i%6 == 0 {
			if i%12 == 0 {
				notes = "line one\nline two"
			} else {
				notes = `He said "hi", then left`
			}
		}
		if _, err := e.owner.Exec(ctx, `INSERT INTO email_data (id, name, email, source_link_id, created_at, is_used, tags, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, i, name, email, link, createdAt, i%3 == 0, tags, notes); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{`SELECT setval(pg_get_serial_sequence('links','id'), 30)`, `SELECT setval(pg_get_serial_sequence('email_data','id'), 60)`} {
		if _, err := e.owner.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

// actor maps a recorded case to the user that made the request.
func actor(name string) map[string]string {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return nil
	case name == "list_non_admin_erin" || name == "stats_domains_non_admin":
		return bearer("erin")
	case strings.Contains(name, "non_admin"):
		return bearer("bob")
	}
	return bearer("alice")
}

var exportName = regexp.MustCompile(`^attachment; filename=export_\d{8}_\d{6}\.csv$`)

func (e dataEnv) play(t *testing.T, name string) (*httptest.ResponseRecorder, contract.Fixture) {
	t.Helper()
	fx := contract.Load(t, "datacrm/"+name)
	body := ""
	ctype := ""
	if string(fx.Request.Body) != "" && string(fx.Request.Body) != "null" {
		body, ctype = string(fx.Request.Body), "application/json"
	}
	rec, _ := e.do(fx.Request.Method, fx.Request.Path, ctype, body, actor(name))
	return rec, fx
}

func respOf(rec *httptest.ResponseRecorder) contract.Resp {
	h := map[string]string{}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			h[k] = v
		}
	}
	return contract.Resp{Status: rec.Code, Headers: h, Body: json.RawMessage(rec.Body.Bytes())}
}

// replayAll plays every recorded case in recording order and compares the cases whose
// name satisfies want. Cases Python answered 500 for are skipped (see TestDataPythonCrashesAnswerClientErrors).
func (e dataEnv) replayAll(t *testing.T, want func(string) bool) {
	t.Helper()
	for _, name := range dataFixtureOrder {
		rec, fx := e.play(t, name)
		if fx.Response.Status == 500 || !want(name) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			if strings.HasPrefix(name, "export_") && fx.Response.Status == 200 {
				if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" ||
					!exportName.MatchString(rec.Header().Get("Content-Disposition")) {
					t.Fatalf("export headers: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Content-Disposition"))
				}
				var wantCSV string
				if err := json.Unmarshal(fx.Response.Body, &wantCSV); err != nil {
					t.Fatal(err)
				}
				if rec.Body.String() != wantCSV {
					t.Fatalf("csv bytes differ:\ngot  %q\nwant %q", rec.Body.String(), wantCSV)
				}
				return
			}
			contract.AssertEqual(t, respOf(rec), fx.Response)
		})
	}
}

func TestDataListMatchesFixture(t *testing.T) {
	newDataEnv(t).replayAll(t, func(n string) bool { return strings.HasPrefix(n, "list_") })
}

func TestDataMarkUsedMatchesFixture(t *testing.T) {
	newDataEnv(t).replayAll(t, func(n string) bool { return strings.HasPrefix(n, "mark_used_") })
}

func TestDataPatchMatchesFixture(t *testing.T) {
	newDataEnv(t).replayAll(t, func(n string) bool { return strings.HasPrefix(n, "patch_") })
}

func TestDataDeleteMatchesFixture(t *testing.T) {
	newDataEnv(t).replayAll(t, func(n string) bool { return strings.HasPrefix(n, "delete_") })
}

func TestDataExportMatchesFixtureBytes(t *testing.T) {
	newDataEnv(t).replayAll(t, func(n string) bool { return strings.HasPrefix(n, "export_") })
}

func TestDataStatsMatchFixtures(t *testing.T) {
	newDataEnv(t).replayAll(t, func(n string) bool { return strings.HasPrefix(n, "stats_") })
}

// The 4 recorded 500s are D4 hardening: answered as client errors or valid pages.
func TestDataPythonCrashesAnswerClientErrors(t *testing.T) {
	e := newDataEnv(t)
	admin := bearer("alice")
	for _, p := range []string{"/api/data?limit=-1", "/api/data?offset=-1"} {
		rec, _ := e.do("GET", p, "", "", admin)
		var b struct {
			Detail []map[string]any `json:"detail"`
		}
		if rec.Code != 422 || json.Unmarshal(rec.Body.Bytes(), &b) != nil || len(b.Detail) != 1 || b.Detail[0]["type"] != "greater_than_equal" {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body)
		}
	}
	for _, p := range []string{"/api/data?sort_by=metadata", "/api/data/export?sort_by=metadata", "/api/data/stats/domains?limit=-1"} {
		if rec, _ := e.do("GET", p, "", "", admin); rec.Code != 200 {
			t.Errorf("%s: %d, want 200 (sort_by falls back to id)", p, rec.Code)
		}
	}
	// a NUL byte cannot be stored: 422, not a driver error
	rec, _ := e.do("GET", "/api/data?q=a%00b", "", "", admin)
	if rec.Code != 422 {
		t.Errorf("NUL in q: %d %s", rec.Code, rec.Body)
	}
	rec, _ = e.do("PATCH", "/api/data/1", "application/json", `{"name":"a\u0000b"}`, admin)
	if rec.Code != 422 {
		t.Errorf("NUL in patch: %d %s", rec.Code, rec.Body)
	}
}

func TestEveryDataRouteRequiresGlobalAdmin(t *testing.T) {
	e := newDataEnv(t)
	// a workspace admin and a plain member: neither is a global admin
	wsa := e.mustUser(t, "wsadmin")
	if _, err := e.owner.Exec(context.Background(), `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ('ws-data-gate','ws-data-gate','ws-data-gate',$1)`, wsa); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM workspace_members WHERE workspace_id='ws-data-gate'`)
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM workspaces WHERE id='ws-data-gate'`)
	})
	if _, err := e.owner.Exec(context.Background(), `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ('ws-data-gate',$1,'admin')`, wsa); err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, p := range e.r.Patterns() {
		if _, path, ok := strings.Cut(p, " "); ok && strings.HasPrefix(path, "/api/data") {
			routes = append(routes, p)
		}
	}
	if len(routes) != 7 {
		t.Fatalf("want 7 /api/data routes, got %d: %v", len(routes), routes)
	}
	for _, p := range routes {
		method, path, _ := strings.Cut(p, " ")
		path = strings.ReplaceAll(path, "{id}", "1")
		body, ctype := "", ""
		if method == "POST" || method == "PATCH" {
			body, ctype = `{}`, "application/json"
		}
		if rec, _ := e.do(method, path, ctype, body, nil); rec.Code != 401 {
			t.Errorf("%s anonymous: %d, want 401", p, rec.Code)
		}
		for _, who := range []string{"wsadmin", "bob", "erin"} {
			rec, got := e.do(method, path, ctype, body, bearer(who))
			if rec.Code != 403 || strings.TrimSpace(string(got.Body)) != `{"detail":"Not authorized"}` {
				t.Errorf("%s as %s: %d %s, want 403 Not authorized", p, who, rec.Code, rec.Body)
			}
		}
	}
	// nothing changed for the refused callers
	var used int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM email_data WHERE is_used`).Scan(&used); err != nil || used != 20 {
		t.Errorf("rows marked used = %d (%v), want the 20 seeded", used, err)
	}
}

func (e dataEnv) mustUser(t *testing.T, name string) int32 {
	t.Helper()
	u, err := e.st.ByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestDataPathIDValidation422(t *testing.T) {
	e := newDataEnv(t)
	admin := bearer("alice")
	for _, c := range [][3]string{{"PATCH", "/api/data/abc", `{"name":"x"}`}, {"DELETE", "/api/data/abc", ""},
		{"PATCH", "/api/data/1.5", `{}`}, {"DELETE", "/api/data/", ""}, {"DELETE", "/api/data/%20", ""}} {
		rec, _ := e.do(c[0], c[1], "application/json", c[2], admin)
		if c[1] == "/api/data/" {
			if rec.Code == 200 {
				t.Errorf("%v: unexpected 200", c)
			}
			continue
		}
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"loc":["path","id"]`) {
			t.Errorf("%v: %d %s", c, rec.Code, rec.Body)
		}
	}
	// huge ids exist nowhere: 404 / not found, never a 500
	rec, _ := e.do("PATCH", "/api/data/99999999999999999999", "application/json", `{}`, admin)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "Record with id 99999999999999999999 not found") {
		t.Errorf("huge patch: %d %s", rec.Code, rec.Body)
	}
	rec, _ = e.do("DELETE", "/api/data/99999999999999999999", "", "", admin)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "not found") {
		t.Errorf("huge delete: %d %s", rec.Code, rec.Body)
	}
	// path error and body error are reported together, path first (FastAPI order)
	rec, _ = e.do("PATCH", "/api/data/abc", "application/json", `{"is_used":"maybe"}`, admin)
	if strings.Index(rec.Body.String(), "path") > strings.Index(rec.Body.String(), "body") || strings.Count(rec.Body.String(), "loc") != 2 {
		t.Errorf("combined: %s", rec.Body)
	}
	// /export and /stats are never shadowed by /{id}
	for _, p := range []string{"/api/data/export", "/api/data/stats/timeline", "/api/data/stats/domains"} {
		if rec, _ := e.do("GET", p, "", "", admin); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestDataBadBodiesAre422(t *testing.T) {
	e := newDataEnv(t)
	admin := bearer("alice")
	for _, c := range []struct{ method, path, body, typ string }{
		{"POST", "/api/data/mark-used", "", "missing"},
		{"POST", "/api/data/mark-used", "{", "json_invalid"},
		{"POST", "/api/data/mark-used", "[1]", "model_attributes_type"},
		{"POST", "/api/data/mark-used", `{"ids":5}`, "list_type"},
		{"POST", "/api/data/mark-used", `{"ids":[1.5]}`, "int_from_float"},
		{"POST", "/api/data/mark-used", `{"used":null}`, "bool_type"},
		{"POST", "/api/data/mark-used", `{"q":3}`, "string_type"},
		{"POST", "/api/data/mark-used", `{"source_link_id":"z"}`, "int_parsing"},
		{"PATCH", "/api/data/1", `{"name":5}`, "string_type"},
		{"PATCH", "/api/data/1", "", "missing"},
	} {
		rec, _ := e.do(c.method, c.path, "application/json", c.body, admin)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"type":"`+c.typ+`"`) {
			t.Errorf("%s %s %q: %d %s, want 422 %s", c.method, c.path, c.body, rec.Code, rec.Body, c.typ)
		}
	}
	// lax pydantic: numeric strings and 1.0 are ints, "yes" is a bool
	rec, got := e.do("POST", "/api/data/mark-used", "application/json", `{"ids":["1",2.0],"used":"yes"}`, admin)
	if rec.Code != 200 || !strings.Contains(string(got.Body), `"count":2`) {
		t.Errorf("lax body: %d %s", rec.Code, rec.Body)
	}
}

// bulkExport seeds n rows through SQL (the replay seed is only 60 rows).
func (e dataEnv) bulk(t *testing.T, n int) {
	t.Helper()
	if _, err := e.owner.Exec(context.Background(), `TRUNCATE email_data RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.owner.Exec(context.Background(), `INSERT INTO email_data (name, email, source_link_id, created_at, is_used, tags, notes)
		SELECT 'Name ' || g, 'user' || g || '@example.com', NULL, '2026-08-29T00:00:00+00:00', g % 2 = 0, 'a,b', 'note ' || repeat('x', 40)
		FROM generate_series(1, $1) g`, n); err != nil {
		t.Fatal(err)
	}
}

// countingWriter is a ResponseWriter that discards the body and tracks heap growth.
type countingWriter struct {
	hdr          http.Header
	status       int
	writes       int
	bytes        int64
	baseline     uint64
	peak         uint64
	onWrite      func(w *countingWriter) error
	firstWriteAt int64
}

func (c *countingWriter) Header() http.Header { return c.hdr }
func (c *countingWriter) WriteHeader(s int)   { c.status = s }
func (c *countingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	c.writes++
	c.bytes += int64(len(p))
	if c.writes%500 == 1 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > c.peak {
			c.peak = m.HeapAlloc
		}
	}
	if c.onWrite != nil {
		if err := c.onWrite(c); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func TestDataExportStreamsWithoutBufferingAll(t *testing.T) {
	e := newDataEnv(t)
	const n = 100_000
	e.bulk(t, n)
	req := httptest.NewRequest("GET", "/api/data/export", nil)
	for k, v := range bearer("alice") {
		req.Header.Set(k, v)
	}
	cw := &countingWriter{hdr: http.Header{}}
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	cw.baseline = m.HeapAlloc
	e.h.ServeHTTP(cw, req)
	if cw.status != 200 || cw.hdr.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("status %d %v", cw.status, cw.hdr)
	}
	// header line + n rows, each row well over 60 bytes
	if cw.bytes < n*60 {
		t.Fatalf("only %d bytes for %d rows", cw.bytes, n)
	}
	if cw.writes < n/1000 {
		t.Errorf("%d writes: the body was not streamed in batches", cw.writes)
	}
	// All rows held at once would cost well over 25 MiB (100k Row structs with 6 strings each plus the
	// CSV text); a streamed export holds one 1000-row batch.
	if grow := int64(cw.peak) - int64(cw.baseline); grow > 25<<20 {
		t.Errorf("heap grew %d MiB during export: rows are being buffered", grow>>20)
	}
}

func TestDataExportClientDisconnectStops(t *testing.T) {
	e := newDataEnv(t)
	e.bulk(t, 20_000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/api/data/export", nil).WithContext(ctx)
	for k, v := range bearer("alice") {
		req.Header.Set(k, v)
	}
	cw := &countingWriter{hdr: http.Header{}}
	cw.onWrite = func(c *countingWriter) error {
		if c.bytes > 100_000 { // the client goes away after the first batch or so
			cancel()
			return context.Canceled
		}
		return nil
	}
	done := make(chan struct{})
	go func() { defer close(done); e.h.ServeHTTP(cw, req) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler still running after the client disconnected")
	}
	if cw.bytes > 1_000_000 { // the full export is ~1.8 MB
		t.Errorf("wrote %d bytes after disconnect", cw.bytes)
	}
	// and the connection pool is intact: the next request works
	if rec, _ := e.do("GET", "/api/data?limit=1", "", "", bearer("alice")); rec.Code != 200 {
		t.Errorf("after disconnect: %d", rec.Code)
	}
}

func TestDataExportBadInputBeforeAnyByteIs422(t *testing.T) {
	e := newDataEnv(t)
	rec, _ := e.do("GET", "/api/data/export?is_used=maybe", "", "", bearer("alice"))
	if rec.Code != 422 || strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") || bytes.Contains(rec.Body.Bytes(), []byte("Name,Email")) {
		t.Errorf("%d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
}
