//go:build integration

package api

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// mintTicket calls POST /api/events/ticket as user (bearer) for ws (X-Workspace-Id).
func (e eventsEnv) mintTicket(t *testing.T, user, ws string, amr []string) (int, string, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/events/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+e.tokenQuery(t, user, amr))
	if ws != "" {
		req.Header.Set("X-Workspace-Id", ws)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var b struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if rec.Code == 200 && b.ExpiresIn != 60 {
		t.Errorf("expires_in %d", b.ExpiresIn)
	}
	return rec.Code, b.Ticket, rec
}

// S-L23: a ticket opens the stream once; the second use is refused.
func TestEventsTicketSingleUse(t *testing.T) {
	setHeartbeat(t, time.Hour)
	e := newEventsEnv(t, nil)
	ws := e.seedWS(t, "t23a")
	code, tk, rec := e.mintTicket(t, "t23a-alice", ws, []string{"pwd"})
	if code != 200 || tk == "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("mint %d %s cc=%q", code, rec.Body, rec.Header().Get("Cache-Control"))
	}
	if strings.Contains(tk, "t23a-alice") {
		t.Fatal("ticket carries the username")
	}
	resp, _, _ := openStream(t, e, "/api/events?ticket="+url.QueryEscape(tk), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("first use %d", resp.StatusCode)
	}
	if rec := e.get("/api/events?ticket=" + url.QueryEscape(tk)); rec.Code != 401 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Invalid ticket"}` {
		t.Fatalf("reuse %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"x", "v1.e30.AAAA", tk + "x"} {
		if rec := e.get("/api/events?ticket=" + url.QueryEscape(bad)); rec.Code != 401 {
			t.Errorf("forged %q: %d", bad, rec.Code)
		}
	}
}

func TestEventsTicketExpires(t *testing.T) {
	e := newEventsEnv(t, nil)
	ws := e.seedWS(t, "t23e")
	_, tk, _ := e.mintTicket(t, "t23e-alice", ws, []string{"pwd"})
	old := ticketNow
	ticketNow = func() time.Time { return time.Now().Add(61 * time.Second) }
	defer func() { ticketNow = old }()
	if rec := e.get("/api/events?ticket=" + url.QueryEscape(tk)); rec.Code != 401 || !strings.Contains(rec.Body.String(), "Invalid ticket") {
		t.Fatalf("expired %d %s", rec.Code, rec.Body)
	}
}

// The ticket is bound to the workspace it was minted for, and minting needs membership.
func TestEventsTicketBoundToWorkspace(t *testing.T) {
	e := newEventsEnv(t, nil)
	ws := e.seedWS(t, "t23w")
	other := e.seedWS(t, "t23x")
	_, tk, _ := e.mintTicket(t, "t23w-alice", ws, []string{"pwd"})
	if rec := e.get("/api/events?ticket=" + url.QueryEscape(tk) + "&workspace_id=" + other); rec.Code != 403 ||
		strings.TrimSpace(rec.Body.String()) != `{"detail":"Workspace access denied"}` {
		t.Fatalf("other ws %d %s", rec.Code, rec.Body)
	}
	if code, _, rec := e.mintTicket(t, "t23w-bob", ws, []string{"pwd"}); code != 403 {
		t.Fatalf("non-member mint %d %s", code, rec.Body)
	}
	// a user deactivated after minting cannot use the ticket.
	_, tk, _ = e.mintTicket(t, "t23w-alice", ws, []string{"pwd"})
	if _, err := e.pool.Exec(t.Context(), `UPDATE users SET is_active=false WHERE username='t23w-alice'`); err != nil {
		t.Fatal(err)
	}
	if rec := e.get("/api/events?ticket=" + url.QueryEscape(tk)); rec.Code != 401 {
		t.Fatalf("inactive %d %s", rec.Code, rec.Body)
	}
}

// The ticket keeps the minting token's amr, so an SSO-enforced workspace still needs an SSO login.
func TestEventsTicketKeepsSSOEnforcement(t *testing.T) {
	setHeartbeat(t, time.Hour)
	e := newEventsEnv(t, nil)
	ws := e.seedWS(t, "t23s")
	bob := e.mustUserByName(t, "t23s-bob")
	e.member(t, ws, bob.ID, "member")
	setSetting(t, e.pool, ws, "OIDC_CONFIG", `{"enforce_sso":true}`)
	// the pwd token cannot mint (workspace middleware) or, if it could, open.
	if code, _, _ := e.mintTicket(t, "t23s-bob", ws, []string{"pwd"}); code != 403 {
		t.Fatalf("pwd mint %d", code)
	}
	_, tk, _ := e.mintTicket(t, "t23s-bob", ws, []string{"sso"})
	resp, _, _ := openStream(t, e, "/api/events?ticket="+url.QueryEscape(tk), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("sso ticket %d", resp.StatusCode)
	}
}

// After the deprecation window a JWT in ?token= is refused; inside it, it still works.
func TestEventsQueryTokenRefusedAfterWindow(t *testing.T) {
	setHeartbeat(t, time.Hour)
	for _, c := range []struct {
		until string
		want  int
	}{{"2000-01-01", 401}, {"2999-01-01T00:00:00Z", 200}, {"not-a-date", 401}} {
		// one env per subtest: testpg holds its cross-package lock until the (sub)test ends.
		t.Run(c.until, func(t *testing.T) {
			e := newEventsEnv(t, func(d *Deps) {
				d.Getenv = func(k string) string {
					if k == "SSE_QUERY_TOKEN_UNTIL" {
						return c.until
					}
					return ""
				}
			})
			ws := e.seedWS(t, "t23q")
			tok := e.tokenQuery(t, "t23q-alice", []string{"pwd"})
			if c.want == 401 {
				rec := e.get("/api/events?token=" + tok + "&workspace_id=" + ws)
				if rec.Code != 401 || !strings.Contains(rec.Body.String(), "/api/events/ticket") {
					t.Errorf("%s: %d %s", c.until, rec.Code, rec.Body)
				}
				return
			}
			resp, _, _ := openStream(t, e, "/api/events?token="+tok+"&workspace_id="+ws, nil)
			if resp.StatusCode != 200 {
				t.Errorf("%s: %d", c.until, resp.StatusCode)
			}
		})
	}
}
