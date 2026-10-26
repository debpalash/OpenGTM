//go:build integration

package api

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

const suppBase = "/api/outreach/suppressions"

type suppEnv struct {
	oseqEnv
	tt *testing.T
}

// newSuppEnv seeds the contract suppression rows (ids 1-6 ws-alpha, 7 ws-beta) and serves only the suppression routes.
func newSuppEnv(t *testing.T) suppEnv {
	t.Helper()
	e := newLeadsEnv(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	oactPurge()
	t.Cleanup(oactPurge)
	for _, r := range oactSuppress {
		ownerExec(t, `INSERT INTO outreach_suppressions (workspace_id, email, reason, source, locked, created_at) VALUES ($1,$2,$3,$4,$5,$6::timestamptz)`, r...)
	}
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Flags: config.Flags{PGLeadStore: true}}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	registerOutreachSuppressions(r, d)
	e.h = r.Handler()
	return suppEnv{oseqEnv{e}, t}
}

func suppActor(name string) (string, string) {
	switch {
	case name == "suppressions_list_beta_bob":
		return "bob", "ws-beta"
	case strings.HasSuffix(name, "as_editor_403"):
		return "erin", ""
	case strings.HasSuffix(name, "no_auth_401"):
		return "", ""
	}
	return "alice", ""
}

// suppRows reduces a list body to the stable columns (ids and timestamps differ per run).
func suppRows(t *testing.T, raw []byte) []string {
	t.Helper()
	var b struct {
		Suppressions []map[string]any `json:"suppressions"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, s := range b.Suppressions {
		out = append(out, strings.Join([]string{s["email"].(string), s["reason"].(string), s["source"].(string), map[bool]string{true: "L", false: "-"}[s["locked"].(bool)]}, "|"))
	}
	return out
}

// suppDeviations: Go refuses what Python stored (an empty key); see the report.
var suppDeviations = map[string]int{"suppress_add_empty_email": 422}

func TestSuppressionRoutesMatchFixtures(t *testing.T) {
	e := newSuppEnv(t)
	order := strings.Fields(`suppressions_list_seed suppress_add_new suppress_add_duplicate suppress_add_gmail_variant suppress_add_idn_domain
		suppress_add_no_at suppress_add_empty_email suppress_add_missing_body_422 suppress_add_as_editor_403 suppress_add_no_auth_401
		suppressions_list_after_add suppressions_list_beta_bob suppress_remove_ok suppress_remove_again_404 suppress_remove_case_insensitive
		suppress_remove_plus_encoded suppress_remove_unknown_404 suppress_remove_foreign_404 suppress_remove_locked_unsub_403
		suppress_remove_locked_complaint_403 suppress_remove_as_editor_403 suppress_remove_no_auth_401 suppressions_list_after_remove`)
	for _, name := range order {
		fx := contract.Load(t, "outreach/"+name)
		who, ws := suppActor(name)
		body := ""
		if b := strings.TrimSpace(string(fx.Request.Body)); b != "" && b != "null" {
			body = b
		}
		rec := e.call(who, ws, fx.Request.Method, fx.Request.Path, body)
		t.Run(name, func(t *testing.T) {
			if want, ok := suppDeviations[name]; ok {
				if rec.Code != want || !strings.Contains(rec.Body.String(), `"detail"`) {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
				return
			}
			if strings.HasPrefix(name, "suppressions_list") {
				if rec.Code != 200 {
					t.Fatalf("%d", rec.Code)
				}
				got, want := suppRows(t, rec.Body.Bytes()), suppRows(t, fx.Response.Body)
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
				}
				return
			}
			var gotBody, wantBody any
			_ = json.Unmarshal(rec.Body.Bytes(), &gotBody)
			_ = json.Unmarshal(fx.Response.Body, &wantBody)
			gb, _ := json.Marshal(gotBody)
			wb, _ := json.Marshal(wantBody)
			got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: gb}
			if v := rec.Header().Get("Content-Type"); v != "" {
				got.Headers["Content-Type"] = v
			}
			want := fx.Response
			want.Body = wb
			contract.AssertEqual(t, got, want)
		})
	}
}

func (e suppEnv) add(email string) (int, map[string]any) {
	b, _ := json.Marshal(map[string]string{"email": email})
	c, m, _ := e.as("alice", "", "POST", suppBase, string(b))
	return c, m
}

func (e suppEnv) count(ws string) int {
	return e.rowCount(e.tt, `SELECT count(*) FROM outreach_suppressions WHERE workspace_id = $1`, ws)
}

func TestAddDuplicateReturnsAddedFalse(t *testing.T) {
	e := newSuppEnv(t)
	if c, m := e.add("Dup@Example.com"); c != 200 || m["added"] != true {
		t.Fatalf("%d %v", c, m)
	}
	if c, m := e.add(" dup@EXAMPLE.com "); c != 200 || m["added"] != false || m["status"] != "ok" {
		t.Fatalf("%d %v", c, m)
	}
	if c, m := e.add("locked-unsub@example.com"); c != 200 || m["added"] != false {
		t.Fatalf("%d %v", c, m)
	}
	var locked bool
	if err := wcrudOwnerQueryRow(t, `SELECT locked FROM outreach_suppressions WHERE email='locked-unsub@example.com'`).Scan(&locked); err != nil || !locked {
		t.Fatalf("locked row altered: %v %v", locked, err)
	}
}

func TestAddGmailVariantsShareKey(t *testing.T) {
	e := newSuppEnv(t)
	// Stored canonical forms differ (no collapsing on write), both are added; match keys collapse on read.
	if c, m := e.add("P.a.t.Lee+news@Gmail.com"); c != 200 || m["added"] != true {
		t.Fatalf("%d %v", c, m)
	}
	var email string
	if err := wcrudOwnerQueryRow(t, `SELECT email FROM outreach_suppressions WHERE email LIKE 'p.a.t%'`).Scan(&email); err != nil || email != "p.a.t.lee+news@gmail.com" {
		t.Fatalf("%q %v", email, err)
	}
	if c, m := e.add("p.a.t.lee+news@googlemail.com"); c != 200 || m["added"] != true {
		t.Fatalf("googlemail is a distinct stored key: %d %v", c, m)
	}
}

func TestRemoveLocked403(t *testing.T) {
	e := newSuppEnv(t)
	for _, em := range []string{"locked-unsub@example.com", "LOCKED-COMPLAINT@example.com"} {
		c, m, raw := e.as("alice", "", "DELETE", suppBase+"/"+url.PathEscape(em), "")
		if c != 403 || m["detail"] != "Cannot remove an unsubscribe/complaint suppression" {
			t.Fatalf("%s %d %s", em, c, raw)
		}
	}
	if n := e.count("ws-alpha"); n != 6 {
		t.Fatalf("rows %d", n)
	}
}

func TestRemoveUnknown404(t *testing.T) {
	e := newSuppEnv(t)
	for _, p := range []string{"nobody%40example.com", "%20", "beta-suppressed%40example.com", strings.Repeat("a", 5000)} {
		c, m, raw := e.as("alice", "", "DELETE", suppBase+"/"+p, "")
		if c != 404 || m["detail"] != "Suppression not found" {
			t.Fatalf("%d %s", c, raw)
		}
	}
	if n := e.count("ws-beta"); n != 1 {
		t.Fatalf("a foreign row must survive: %d", n)
	}
}

func TestRemoveNormalizesPathEmail(t *testing.T) {
	e := newSuppEnv(t)
	ownerExec(t, `INSERT INTO outreach_suppressions (workspace_id, email, reason, source, locked) VALUES
		('ws-alpha','user@xn--bcher-kva.example','manual','manual',false),
		('ws-alpha','case@example.com','manual','manual',false),
		('ws-alpha','a+b@example.com','manual','manual',false)`)
	for _, p := range []string{"user%40B%C3%9CCHER.example", "CASE%40Example.COM", "a%2Bb%40example.com", "%20Manual-Removable%40Example.com%20"} {
		c, m, raw := e.as("alice", "", "DELETE", suppBase+"/"+p, "")
		if c != 200 || m["status"] != "ok" {
			t.Fatalf("%s: %d %s", p, c, raw)
		}
	}
	if n := e.count("ws-alpha"); n != 5 {
		t.Fatalf("rows %d", n)
	}
}

func TestEmptyEmail422(t *testing.T) {
	e := newSuppEnv(t)
	for _, b := range []string{`{"email":""}`, `{"email":"   "}`, `{"email":" "}`, `{"email":null}`, `{"email":5}`, `{"email":"` + strings.Repeat("a", 321) + `"}`, `{"email":"a\u0000b@x.com"}`} {
		c, m, raw := e.as("alice", "", "POST", suppBase, b)
		if _, ok := m["detail"].([]any); c != 422 || !ok {
			t.Fatalf("%s: %d %s", b[:min(len(b), 30)], c, raw)
		}
	}
	if n := e.count("ws-alpha"); n != 6 {
		t.Fatalf("nothing may be stored: %d", n)
	}
	if c, _ := e.add(strings.Repeat("a", 308) + "@example.com"); c != 200 {
		t.Fatalf("320 chars is allowed: %d", c)
	}
}

func TestRequiresAdminForMutations(t *testing.T) {
	e := newSuppEnv(t)
	for _, m := range [][2]string{{"POST", suppBase}, {"DELETE", suppBase + "/manual-removable%40example.com"}} {
		for _, who := range []string{"erin", "bob"} {
			body := ""
			if m[0] == "POST" {
				body = `{"email":"x@example.com"}`
			}
			if c, _, raw := e.as(who, "ws-alpha", m[0], m[1], body); c != 403 && c != 404 {
				t.Fatalf("%s %s: %d %s", who, m[0], c, raw)
			}
		}
		if c, _, _ := e.as("", "", m[0], m[1], `{"email":"x@example.com"}`); c != 401 {
			t.Fatalf("anon %s: %d", m[0], c)
		}
	}
	if n := e.count("ws-alpha"); n != 6 {
		t.Fatalf("rows %d", n)
	}
	if c, _, _ := e.as("erin", "", "GET", suppBase, ""); c != 200 {
		t.Fatalf("any member may list: %d", c)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newSuppEnv(t)
	c, m, raw := e.as("bob", "ws-beta", "POST", suppBase, `{"email":"manual-removable@example.com"}`)
	if c != 200 || m["added"] != true {
		t.Fatalf("same email in another workspace is its own row: %d %s", c, raw)
	}
	if c, _, _ := e.as("bob", "ws-beta", "DELETE", suppBase+"/locked-unsub%40example.com", ""); c != 404 {
		t.Fatalf("beta must not see alpha rows: %d", c)
	}
	if c, _, _ := e.as("bob", "ws-beta", "DELETE", suppBase+"/manual-removable%40example.com", ""); c != 200 {
		t.Fatalf("%d", c)
	}
	if n := e.count("ws-alpha"); n != 6 {
		t.Fatalf("alpha rows %d", n)
	}
	_, _, raw2 := e.as("bob", "ws-beta", "GET", suppBase, "")
	if strings.Contains(raw2, "alpha") || strings.Contains(raw2, "locked-") {
		t.Fatalf("leak: %s", raw2)
	}
}
