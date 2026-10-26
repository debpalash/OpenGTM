//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
)

// Plan 10 Task 16: POST /{id}/refresh, GET /{id}/members, GET /{id}/events against the recorded Python fixtures
// (testdata/contract/audiences/aud_{refresh,members,events}_*) over the plan 10 seed.

// readActor maps a fixture to its recorder (headers are redacted). The "beta_own" ones were read by the ws-beta owner.
func readActor(name string) (user, ws string) {
	if strings.Contains(name, "beta_own") {
		return "bob", "ws-beta"
	}
	return audFixtureActor(name)
}

func (e audEnv) readReplay(t *testing.T, name string, ids map[string]string, opts ...contract.Option) contract.Resp {
	t.Helper()
	fx := contract.Load(t, "audiences/"+name)
	user, ws := readActor(name)
	path := audIDRe.ReplaceAllStringFunc(fx.Request.Path, func(s string) string {
		if v, ok := ids[s]; ok {
			return v
		}
		return s
	})
	_, got := e.call(user, ws, fx.Request.Method, path, "")
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
	return got
}

// afterRefreshOpts ignores what a refresh regenerates: new row ids and wall-clock timestamps of the listed rows.
func afterRefreshOpts(t *testing.T, name string) []contract.Option {
	t.Helper()
	fx := contract.Load(t, "audiences/"+name)
	var rows []map[string]any
	if err := json.Unmarshal(fx.Response.Body, &rows); err != nil {
		t.Fatal(err)
	}
	var opts []contract.Option
	for i := range rows {
		opts = append(opts, contract.IgnorePaths(fmt.Sprintf("$[%d].id", i)))
		for _, k := range []string{"joined_at", "last_seen_at", "created_at"} {
			if _, ok := rows[i][k]; ok {
				opts = append(opts, contract.IgnoreShapeOf(fmt.Sprintf("$[%d].%s", i, k)))
			}
		}
	}
	return opts
}

func TestRefreshRouteMatchesFixtures(t *testing.T) {
	e := newAudEnv(t)
	ids := map[string]string{}
	// Reads over the untouched seed (exact), then the refreshes in recorded order, then the reads they changed.
	for _, n := range []string{"aud_members_default", "aud_members_act1", "aud_members_act3_empty", "aud_members_beta_own", "aud_events_default", "aud_events_act1", "aud_events_beta_own"} {
		e.readReplay(t, n, ids)
	}
	refreshOpts := func(n string) []contract.Option {
		fx := contract.Load(t, "audiences/"+n)
		opts := []contract.Option{contract.IgnorePaths("$.audience.id")}
		if fx.Response.Status == 200 {
			for _, k := range audTimestampPaths {
				opts = append(opts, contract.IgnoreShapeOf("$.audience."+k))
			}
		}
		return opts
	}
	for _, n := range []string{"aud_refresh_dynamic", "aud_refresh_dynamic_again"} {
		e.readReplay(t, n, ids, refreshOpts(n)...)
	}
	// The recording of the everyone refresh ran after the destination callback cases, which filled these lead fields.
	if _, err := audOwner(t).Exec(context.Background(), `UPDATE leads SET contact_title = CASE id WHEN 14 THEN 'Boss' WHEN 6 THEN 'COO' WHEN 5 THEN 'CTO' END,
		city = CASE WHEN id = 5 THEN 'Berlin' ELSE city END,
		employee_count_exact = CASE WHEN id = 5 THEN 42 ELSE employee_count_exact END WHERE workspace_id = 'ws-alpha' AND id IN (5, 6, 14)`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"aud_refresh_everyone", "aud_refresh_disabled_audience", "aud_refresh_beta_own"} {
		e.readReplay(t, n, ids, refreshOpts(n)...)
	}
	// An editor creates an audience (materialized), then refreshes it: nothing changed.
	rec, _ := e.call("erin", "ws-alpha", "POST", "/api/audiences", `{"name":"Erin audience"}`)
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if rec.Code != 201 || created.ID == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	ids["00000000-0000-4000-8000-000000002058"] = created.ID
	e.readReplay(t, "aud_refresh_created_by_erin", ids, refreshOpts("aud_refresh_created_by_erin")...)
	for _, n := range []string{"aud_refresh_foreign_404", "aud_refresh_unknown_404", "aud_refresh_no_auth", "aud_refresh_viewer_403"} {
		e.readReplay(t, n, ids)
	}
	for _, n := range []string{"aud_members_act1_after_refresh", "aud_members_everyone_after_refresh", "aud_events_act1_after_refresh", "aud_events_everyone_after_refresh"} {
		e.readReplay(t, n, ids, afterRefreshOpts(t, n)...)
	}
}

// holdAudienceLock locks the audience row from another connection until release is called.
func holdAudienceLock(t *testing.T, id string) (release func()) {
	t.Helper()
	ctx := context.Background()
	c := audOwner(t)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM audiences WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	var done atomic.Bool
	return func() {
		if done.CompareAndSwap(false, true) {
			_ = tx.Rollback(ctx)
		}
	}
}

func TestRefreshTimeoutReleasesLock(t *testing.T) {
	e := newAudEnv(t)
	old := audReadRefreshTimeout
	audReadRefreshTimeout = 300 * time.Millisecond
	defer func() { audReadRefreshTimeout = old }()

	release := holdAudienceLock(t, "aud-act-1")
	defer release()
	time.AfterFunc(900*time.Millisecond, release) // the degraded-health write waits for the holder, then lands
	start := time.Now()
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/audiences/aud-act-1/refresh", "")
	if rec.Code != 504 || rec.Body.String() != `{"detail":"audience refresh timed out"}` {
		t.Fatalf("timeout answer: %d %s", rec.Code, rec.Body.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("refresh outlived its bound: %v", d)
	}
	// Nothing of the refresh is left holding the row: it can be locked at once and the next refresh runs.
	ctx := context.Background()
	c := audOwner(t)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM audiences WHERE id = 'aud-act-1' FOR UPDATE NOWAIT`); err != nil {
		t.Errorf("row lock still held after the 504: %v", err)
	}
	_ = tx.Rollback(ctx)
	if n := audOwnerInt(t, `SELECT count(*) FROM audience_members WHERE audience_id = 'aud-act-1' AND workspace_id = 'ws-alpha'`); n != 2 {
		t.Errorf("a timed-out refresh changed membership: %d", n)
	}
	audReadRefreshTimeout = old
	rec, _ = e.call("alice", "ws-alpha", "POST", "/api/audiences/aud-act-1/refresh", "")
	if rec.Code != 200 {
		t.Fatalf("refresh after the timeout: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRefreshSingleFlightPerAudience(t *testing.T) {
	e := newAudEnv(t)
	codes := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func() {
			rec, _ := e.call("alice", "ws-alpha", "POST", "/api/audiences/aud-act-2/refresh", "")
			codes <- rec.Code
		}()
	}
	for i := 0; i < 4; i++ {
		if c := <-codes; c != 200 {
			t.Errorf("concurrent refresh: %d", c)
		}
	}
	// 15 matching leads, one row each however the refreshes interleaved.
	if n := audOwnerInt(t, `SELECT count(*) FROM audience_members WHERE audience_id = 'aud-act-2'`); n != 15 {
		t.Errorf("members after concurrent refreshes: %d", n)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM (SELECT lead_id FROM audience_members WHERE audience_id = 'aud-act-2' GROUP BY lead_id HAVING count(*) > 1) d`); n != 0 {
		t.Errorf("duplicate members: %d", n)
	}
}

func replayAll(t *testing.T, e audEnv, names []string) {
	t.Helper()
	for _, n := range names {
		e.readReplay(t, n, nil)
	}
}

// assertIDsDescend asserts ids strictly descend and returns them.
func assertIDsDescend(t *testing.T, body []byte) []int {
	t.Helper()
	var rows []struct{ ID int }
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	var out []int
	for i, r := range rows {
		if i > 0 && r.ID >= rows[i-1].ID {
			t.Errorf("ids not descending: %v", rows)
		}
		out = append(out, r.ID)
	}
	return out
}

func TestMembersPaginationAndOrder(t *testing.T) {
	e := newAudEnv(t)
	replayAll(t, e, []string{"aud_members_default", "aud_members_limit3", "aud_members_limit3_offset3", "aud_members_before_id_5",
		"aud_members_before_id_5_limit2", "aud_members_before_id_and_offset", "aud_members_limit1000", "aud_members_offset_past_end",
		"aud_members_act1", "aud_members_act3_empty", "aud_members_beta_own", "aud_members_unknown_404"})
	rec, _ := e.call("alice", "ws-alpha", "GET", "/api/audiences/aud-act-2/members?limit=1000", "")
	all := assertIDsDescend(t, rec.Body.Bytes())
	if len(all) != 10 {
		t.Fatalf("seed members: %d", len(all))
	}
	// before_id is exclusive, offset applies after it, limit last.
	rec, _ = e.call("alice", "ws-alpha", "GET", fmt.Sprintf("/api/audiences/aud-act-2/members?before_id=%d&offset=1&limit=2", all[2]), "")
	got := assertIDsDescend(t, rec.Body.Bytes())
	if fmt.Sprint(got) != fmt.Sprint(all[4:6]) {
		t.Errorf("before_id+offset+limit: %v want %v", got, all[4:6])
	}
	// Item key order is Python's to_api().
	rec, _ = e.call("alice", "ws-alpha", "GET", "/api/audiences/aud-act-2/members?limit=1", "")
	if keys := firstObjectKeys(t, rec.Body.Bytes()); strings.Join(keys, ",") != "id,lead_id,snapshot,joined_at,last_seen_at" {
		t.Errorf("member key order: %v", keys)
	}
	// An id beyond int32 is a plain filter, not a database error.
	if rec, _ = e.call("alice", "ws-alpha", "GET", "/api/audiences/aud-act-2/members?before_id=99999999999999999999", ""); rec.Code != 200 {
		t.Errorf("huge before_id: %d %s", rec.Code, rec.Body.String())
	}
}

func firstObjectKeys(t *testing.T, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, _ := dec.Token(); tok != json.Delim('[') {
		t.Fatalf("not a list: %s", body)
	}
	if tok, _ := dec.Token(); tok != json.Delim('{') {
		t.Fatalf("no first object: %s", body)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestMembersBounds422(t *testing.T) {
	e := newAudEnv(t)
	replayAll(t, e, []string{"aud_members_limit0_422", "aud_members_limit1001_422", "aud_members_limit_text_422", "aud_members_before_id0_422",
		"aud_members_before_id_text_422", "aud_members_offset_neg_422", "aud_members_offset100001_422"})
	// Every bad parameter is reported, in declaration order; validation precedes the 404.
	rec, _ := e.call("alice", "ws-alpha", "GET", "/api/audiences/does-not-exist/members?limit=0&before_id=0&offset=-1", "")
	var out struct {
		Detail []struct{ Loc []any }
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 422 || len(out.Detail) != 3 || out.Detail[0].Loc[1] != "limit" || out.Detail[1].Loc[1] != "before_id" || out.Detail[2].Loc[1] != "offset" {
		t.Errorf("multi-error: %d %s", rec.Code, rec.Body.String())
	}
	// Boundaries are accepted.
	for _, q := range []string{"limit=1", "limit=1000", "offset=0", "offset=100000", "before_id=1", "limit=+5", "limit=5.0", "limit=%205"} {
		if rec, _ := e.call("alice", "ws-alpha", "GET", "/api/audiences/aud-act-2/members?"+q, ""); rec.Code != 200 {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	// Last value wins for a repeated parameter (FastAPI scalar).
	rec, _ = e.call("alice", "ws-alpha", "GET", "/api/audiences/aud-act-2/members?limit=1&limit=2", "")
	var rows []any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 2 {
		t.Errorf("repeated limit: %d rows", len(rows))
	}
}

func TestEventsPaginationAndOrder(t *testing.T) {
	e := newAudEnv(t)
	replayAll(t, e, []string{"aud_events_default", "aud_events_limit3", "aud_events_limit500", "aud_events_before_id_10_limit3", "aud_events_before_id_1",
		"aud_events_act1", "aud_events_act3", "aud_events_beta_own", "aud_events_unknown_404"})
	rec, _ := e.call("alice", "ws-alpha", "GET", "/api/audiences/aud-act-2/events?limit=500", "")
	all := assertIDsDescend(t, rec.Body.Bytes())
	if len(all) != 12 {
		t.Fatalf("seed events: %d", len(all))
	}
	rec, _ = e.call("alice", "ws-alpha", "GET", fmt.Sprintf("/api/audiences/aud-act-2/events?before_id=%d&limit=2", all[3]), "")
	if got := assertIDsDescend(t, rec.Body.Bytes()); fmt.Sprint(got) != fmt.Sprint(all[4:6]) {
		t.Errorf("before_id+limit: %v want %v", got, all[4:6])
	}
	rec, _ = e.call("alice", "ws-alpha", "GET", "/api/audiences/aud-act-2/events?limit=1", "")
	if keys := firstObjectKeys(t, rec.Body.Bytes()); strings.Join(keys, ",") != "id,audience_id,lead_id,event_type,snapshot,created_at" {
		t.Errorf("event key order: %v", keys)
	}
}

func TestEventsBounds422(t *testing.T) {
	e := newAudEnv(t)
	replayAll(t, e, []string{"aud_events_limit0_422", "aud_events_limit501_422", "aud_events_limit_text_422", "aud_events_before_id0_422"})
	for _, q := range []string{"limit=1", "limit=500", "before_id=1", "offset=-5"} { // events declares no offset: ignored
		if rec, _ := e.call("alice", "ws-alpha", "GET", "/api/audiences/aud-act-2/events?"+q, ""); rec.Code != 200 {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
}

func TestReadsViewerAllowedRefreshEditorOnly(t *testing.T) {
	e := newAudEnv(t)
	e.readReplay(t, "aud_members_viewer_bob_alpha", nil)
	if rec, _ := e.call("bob", "ws-alpha", "GET", "/api/audiences/aud-act-2/events", ""); rec.Code != 200 {
		t.Errorf("viewer events: %d", rec.Code)
	}
	before := audOwnerInt(t, `SELECT count(*) FROM audience_members WHERE audience_id = 'aud-act-2'`)
	e.readReplay(t, "aud_refresh_viewer_403", nil)
	e.readReplay(t, "aud_refresh_no_auth", nil)
	for _, p := range []string{"members", "events"} {
		e.readReplay(t, "aud_"+p+"_no_auth", nil)
	}
	if audOwnerInt(t, `SELECT count(*) FROM audience_members WHERE audience_id = 'aud-act-2'`) != before {
		t.Error("a forbidden refresh changed membership")
	}
	if rec, _ := e.call("erin", "ws-alpha", "POST", "/api/audiences/aud-act-2/refresh", ""); rec.Code != 200 {
		t.Errorf("editor refresh: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := e.call("erin", "ws-beta", "GET", "/api/audiences/aud-beta-act/members", ""); rec.Code != 403 {
		t.Errorf("non-member read: %d", rec.Code)
	}
}

func TestAudienceReadsForeignWorkspace404(t *testing.T) {
	e := newAudEnv(t)
	replayAll(t, e, []string{"aud_members_foreign_404", "aud_events_foreign_404", "aud_refresh_foreign_404"})
	want := `{"detail":"Audience not found"}`
	for _, c := range []struct{ who, ws, method, path string }{
		{"bob", "ws-beta", "GET", "/api/audiences/aud-act-1/members"},
		{"bob", "ws-beta", "GET", "/api/audiences/aud-act-1/events"},
		{"bob", "ws-beta", "POST", "/api/audiences/aud-act-1/refresh"},
	} {
		if rec, _ := e.call(c.who, c.ws, c.method, c.path, ""); rec.Code != 404 || rec.Body.String() != want {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// The foreign refresh neither ran nor recorded health on the owner's row.
	if n := audOwnerInt(t, `SELECT count(*) FROM audiences WHERE id = 'aud-act-1' AND refresh_health = 'degraded'`); n != 0 {
		t.Error("foreign refresh touched the audience")
	}
}

// A literal path segment must not be shadowed by {audience_id}, nor shadow it: audiences named like the sub-routes.
func TestAudienceReadRoutePrecedence(t *testing.T) {
	e := newAudEnv(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/audiences/members/members"}, {"GET", "/api/audiences/events/events"}, {"POST", "/api/audiences/refresh/refresh"},
		{"GET", "/api/audiences/refresh/members"},
	} {
		_, got := e.call("alice", "ws-alpha", c.method, c.path, "")
		if got.Status != 404 || string(got.Body) != `{"detail":"Audience not found"}` {
			t.Errorf("%s %s: %d %s", c.method, c.path, got.Status, got.Body)
		}
	}
}
