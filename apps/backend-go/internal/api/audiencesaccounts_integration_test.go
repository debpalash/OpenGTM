//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
)

// Plan 10 Task 17: GET /api/audiences/{id}/accounts against the recorded Python fixtures
// (testdata/contract/audiences/aud_accounts_*) plus the rollup, bounds and tenancy properties.

type accMember struct {
	lead int
	snap string
}

// accSeed inserts an audience with the given members (member ids come from the sequence, which the seed loader moved past the seeded ids).
func accSeed(t *testing.T, ws, aud string, members []accMember) {
	t.Helper()
	ctx := context.Background()
	c := audOwner(t)
	if _, err := c.Exec(ctx, `INSERT INTO audiences (id, workspace_id, name, filters, member_count) VALUES ($1, $2, $1, '{}'::json, $3)`,
		aud, ws, len(members)); err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if _, err := c.Exec(ctx, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot) VALUES ($1, $2, $3, $4::json)`,
			ws, aud, m.lead, m.snap); err != nil {
			t.Fatal(err)
		}
	}
}

func accSignal(t *testing.T, ws, id string, lead int, weight any) {
	t.Helper()
	if _, err := audOwner(t).Exec(context.Background(),
		`INSERT INTO signals (id, workspace_id, lead_id, company, signal_type, title, weight, created_at) VALUES ($1, $2, $3, 'x', 'funding', 't', $4, 1788000000)`,
		id, ws, lead, weight); err != nil {
		t.Fatal(err)
	}
}

func (e audEnv) accounts(t *testing.T, who, ws, path string) (int, map[string]any, string) {
	t.Helper()
	rec, _ := e.call(who, ws, "GET", path, "")
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Body.String()
}

func accKeys(m map[string]any) []string {
	var out []string
	list, _ := m["accounts"].([]any)
	for _, a := range list {
		am, _ := a.(map[string]any)
		k, _ := am["key"].(string)
		out = append(out, k)
	}
	return out
}

func TestAccountsMatchesFixtures(t *testing.T) {
	e := newAudEnv(t)
	replay := func(name string) {
		fx := contract.Load(t, "audiences/"+name)
		user, ws := "alice", "ws-alpha"
		switch {
		case strings.Contains(name, "no_auth"):
			user, ws = "", ""
		case strings.Contains(name, "beta_own"):
			user, ws = "bob", "ws-beta"
		}
		_, got := e.call(user, ws, "GET", fx.Request.Path, "")
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	for _, n := range audFixtureNames(t, "aud_accounts_") {
		if n != "aud_accounts_everyone_after_refresh" {
			replay(n)
		}
	}
	// The recording refreshed the "Everyone" audience (10 -> 15 members) before this read, after earlier
	// fixtures had titled leads 14, 6 and 5 (the seed's lead rows have no title; the member snapshots do).
	for id, title := range map[int]string{14: "Boss", 6: "COO", 5: "CTO"} {
		if _, err := audOwner(t).Exec(context.Background(), `UPDATE leads SET contact_title = $1 WHERE id = $2 AND workspace_id = 'ws-alpha'`, title, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := audiencerefresh.Refresh(context.Background(), e.pool, "ws-alpha", "aud-act-2", audiencerefresh.Deps{
		AudienceEvents: nopEmitter{}, ExecutorFor: func(string) string { return "python" }, PageSize: 500}); err != nil {
		t.Fatal(err)
	}
	replay("aud_accounts_everyone_after_refresh")
}

func TestAccountsResponseKeyOrder(t *testing.T) {
	e := newAudEnv(t)
	code, _, raw := e.accounts(t, "alice", "ws-alpha", "/api/audiences/aud-act-1/accounts")
	if code != 200 {
		t.Fatal(code, raw)
	}
	for _, want := range []string{`{"accounts":[{"key":`, `"company":`, `"website":`, `"contacts":`, `"with_email":`, `"with_phone":`,
		`"decision_makers":`, `"signal_count":`, `"signal_weight":`, `"profiles":[{"lead_id":`, `"avg_score":`, `"email_coverage_pct":`,
		`"phone_coverage_pct":`, `"summary":{"account_count":`, `"contact_count":`, `"accounts_with_signals":`, `"accounts_with_decision_makers":`,
		`"pagination":{"limit":50,"offset":0,"next_offset":null,"total":2}`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
	idx := func(s string) int { return strings.Index(raw, s) }
	order := []string{`"key"`, `"company"`, `"website"`, `"contacts"`, `"with_email"`, `"with_phone"`, `"decision_makers"`, `"signal_count"`,
		`"signal_weight"`, `"profiles"`, `"avg_score"`, `"email_coverage_pct"`, `"phone_coverage_pct"`, `"summary"`, `"pagination"`}
	for i := 1; i < len(order); i++ {
		if idx(order[i-1]) > idx(order[i]) {
			t.Fatalf("key order %s before %s in %s", order[i], order[i-1], raw)
		}
	}
}

func TestAccountsSortAndPagination(t *testing.T) {
	e := newAudEnv(t)
	accSeed(t, "ws-alpha", "acc-sort", []accMember{
		{9001, `{"company":"Beta Co","website":"https://b.example.com","score":50}`},
		{9002, `{"company":"alpha co","website":"https://www.a.example.com","score":50}`},
		{9003, `{"company":"C","website":"c.example.com","score":80}`},
		{9004, `{"company":"D","website":"d.example.com","score":10}`},
		{9005, `{"company":"E1","website":"e.example.com","score":50}`},
		{9006, `{"company":"E2","website":"e.example.com","score":50}`},
	})
	accSignal(t, "ws-alpha", "acc-sig-1", 9004, 5)
	code, m, raw := e.accounts(t, "alice", "ws-alpha", "/api/audiences/acc-sort/accounts")
	if code != 200 {
		t.Fatal(code, raw)
	}
	want := []string{"d.example.com", "c.example.com", "e.example.com", "a.example.com", "b.example.com"}
	if got := accKeys(m); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v want %v", got, want)
	}
	code, m, _ = e.accounts(t, "alice", "ws-alpha", "/api/audiences/acc-sort/accounts?limit=2&offset=2")
	pg, _ := m["pagination"].(map[string]any)
	if code != 200 || strings.Join(accKeys(m), ",") != "e.example.com,a.example.com" || pg["next_offset"] != float64(4) || pg["total"] != float64(5) {
		t.Fatalf("page 2: %d %v", code, m)
	}
	_, m, _ = e.accounts(t, "alice", "ws-alpha", "/api/audiences/acc-sort/accounts?limit=2&offset=4")
	lastPg, _ := m["pagination"].(map[string]any)
	lastList, _ := m["accounts"].([]any)
	if lastPg["next_offset"] != nil || len(lastList) != 1 {
		t.Fatalf("last page: %v", m)
	}
	if s, _ := m["summary"].(map[string]any); s["account_count"] != float64(5) || s["contact_count"] != float64(6) || s["accounts_with_signals"] != float64(1) {
		t.Fatalf("summary %v", s)
	}
}

func TestAccountsBounds422(t *testing.T) {
	e := newAudEnv(t)
	for _, q := range []string{"limit=0", "limit=201", "offset=-1", "offset=100001", "limit=abc", "limit=1.5", "limit=", "limit=0&offset=-1"} {
		if code, _, raw := e.accounts(t, "alice", "ws-alpha", "/api/audiences/aud-act-2/accounts?"+q); code != 422 {
			t.Errorf("%s: %d %s", q, code, raw)
		}
	}
	// Validation precedes the lookup: a bad limit on an unknown audience is a 422, not a 404.
	if code, _, _ := e.accounts(t, "alice", "ws-alpha", "/api/audiences/nope/accounts?limit=0"); code != 422 {
		t.Errorf("unknown+bad limit: %d", code)
	}
	// Errors are reported in declaration order: limit, then offset.
	_, m, _ := e.accounts(t, "alice", "ws-alpha", "/api/audiences/aud-act-2/accounts?offset=-1&limit=0")
	if raw, _ := json.Marshal(m["detail"]); !regexp.MustCompile(`"limit".*"offset"`).Match(raw) {
		t.Errorf("detail order %s", raw)
	}
	// pydantic lax ints are accepted.
	for _, q := range []string{"limit=10.0", "limit=%201", "limit=1_0", "limit=%2B5", "offset=0.0"} {
		if code, _, raw := e.accounts(t, "alice", "ws-alpha", "/api/audiences/aud-act-2/accounts?"+q); code != 200 {
			t.Errorf("%s: %d %s", q, code, raw)
		}
	}
}

func TestAccountsEmptyAudience(t *testing.T) {
	e := newAudEnv(t)
	code, _, raw := e.accounts(t, "alice", "ws-alpha", "/api/audiences/aud-act-3/accounts")
	want := `{"accounts":[],"summary":{"account_count":0,"contact_count":0,"accounts_with_signals":0,"accounts_with_decision_makers":0},"pagination":{"limit":50,"offset":0,"next_offset":null,"total":0}}`
	if code != 200 || strings.TrimSpace(raw) != want {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestAccountsSignalAggregation(t *testing.T) {
	e := newAudEnv(t)
	accSeed(t, "ws-alpha", "acc-sig", []accMember{
		{9101, `{"company":"S","website":"s.example.com","score":0}`},
		{9102, `{"company":"S","website":"s.example.com"}`},
		{9103, `{"company":"T","website":"t.example.com"}`},
	})
	accSignal(t, "ws-alpha", "acc-s1", 9101, 7)
	accSignal(t, "ws-alpha", "acc-s2", 9101, 3)
	accSignal(t, "ws-alpha", "acc-s3", 9102, nil) // NULL weight counts as a signal, adds 0
	accSignal(t, "ws-alpha", "acc-s4", 9199, 50)  // lead outside the audience
	accSignal(t, "ws-beta", "acc-s5", 9103, 40)   // same lead id, other workspace
	_, m, raw := e.accounts(t, "alice", "ws-alpha", "/api/audiences/acc-sig/accounts")
	accts, _ := m["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatal(raw)
	}
	s, _ := accts[0].(map[string]any)
	tt, _ := accts[1].(map[string]any)
	if s["key"] != "s.example.com" || s["signal_count"] != float64(3) || s["signal_weight"] != float64(10) || s["contacts"] != float64(2) {
		t.Fatalf("s: %v", s)
	}
	if tt["signal_count"] != float64(0) || tt["signal_weight"] != float64(0) {
		t.Fatalf("t: %v", tt)
	}
	if sum, _ := m["summary"].(map[string]any); sum["accounts_with_signals"] != float64(1) {
		t.Fatalf("summary %v", sum)
	}
}

func TestAccountsBadWebsiteNoCrash(t *testing.T) {
	e := newAudEnv(t)
	accSeed(t, "ws-alpha", "acc-bad", []accMember{
		{9201, `{"company":"Bracket Co","website":"http://[bad","score":1}`},
		{9202, `{"company":"Num Co","website":12345}`},
		{9203, `{"company":"Empty Co","website":"   "}`},
		{9204, `{"website":"","contact_title":null,"email":null}`},
		{9205, `{}`},
	})
	code, m, raw := e.accounts(t, "alice", "ws-alpha", "/api/audiences/acc-bad/accounts")
	if code != 200 {
		t.Fatal(code, raw)
	}
	got := strings.Join(accKeys(m), "|")
	for _, k := range []string{"company:bracket co", "company:empty co", "company:unknown account"} {
		if !strings.Contains(got, k) {
			t.Fatalf("%s missing in %s", k, got)
		}
	}
	// F14: a score Python's float() rejects (a 500 there) counts as no score.
	accSeed(t, "ws-alpha", "acc-badscore", []accMember{{9301, `{"company":"Z","score":"abc"}`}, {9302, `{"company":"Z","score":"40"}`}, {9303, `{"company":"Z","score":"nan"}`}})
	code, m, raw = e.accounts(t, "alice", "ws-alpha", "/api/audiences/acc-badscore/accounts")
	if code != 200 {
		t.Fatalf("bad score: %d %s", code, raw)
	}
	if accts, _ := m["accounts"].([]any); len(accts) != 1 || accts[0].(map[string]any)["avg_score"] != float64(13.3) {
		t.Fatalf("bad score rollup: %s", raw)
	}
}

func TestAccountsTooLarge422(t *testing.T) {
	e := newAudEnv(t)
	old := audAccountsMaxMembers
	audAccountsMaxMembers = 3
	t.Cleanup(func() { audAccountsMaxMembers = old })
	var ms []accMember
	for i := 0; i < 4; i++ {
		ms = append(ms, accMember{9400 + i, fmt.Sprintf(`{"company":"C%d"}`, i)})
	}
	accSeed(t, "ws-alpha", "acc-big", ms)
	rec, _ := e.call("alice", "ws-alpha", "GET", "/api/audiences/acc-big/accounts", "")
	if rec.Code != http.StatusUnprocessableEntity || strings.TrimSpace(rec.Body.String()) != `{"detail":"audience too large for account rollup"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	accSeed(t, "ws-alpha", "acc-edge", ms[:3])
	// exactly at the cap is fine (member_count rows are distinct lead ids per audience)
	if code, _, raw := e.accounts(t, "alice", "ws-alpha", "/api/audiences/acc-edge/accounts"); code != 200 {
		t.Fatal(code, raw)
	}
}

func TestAccountsTenantIsolation(t *testing.T) {
	e := newAudEnv(t)
	// alpha's audience from beta's workspace, and beta's from alpha's: 404 either way.
	if code, m, _ := e.accounts(t, "bob", "ws-beta", "/api/audiences/aud-act-2/accounts"); code != 404 || m["detail"] != "Audience not found" {
		t.Fatalf("beta reading alpha: %d %v", code, m)
	}
	if code, _, _ := e.accounts(t, "alice", "ws-alpha", "/api/audiences/aud-beta-act/accounts"); code != 404 {
		t.Fatalf("alpha reading beta: %d", code)
	}
	// A viewer may read (no role gate on this route).
	if code, _, raw := e.accounts(t, "bob", "ws-alpha", "/api/audiences/aud-act-2/accounts"); code != 200 {
		t.Fatalf("viewer: %d %s", code, raw)
	}
	// A non-member of the requested workspace is refused before the lookup.
	if code, _, raw := e.accounts(t, "erin", "ws-beta", "/api/audiences/aud-beta-act/accounts"); code != 403 || !strings.Contains(raw, `"detail":"Workspace access denied"`) {
		t.Fatalf("non-member: %d %s", code, raw)
	}
}
