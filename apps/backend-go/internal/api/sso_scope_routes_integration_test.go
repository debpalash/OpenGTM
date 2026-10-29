//go:build integration

package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func ssoBearer(t *testing.T, name, ws string) map[string]string {
	t.Helper()
	tok, err := testSigner.AccessFor(name, []string{"sso"}, ws)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Authorization": "Bearer " + tok}
}

// /api/workspaces with an SSO session (sso_ws): list and dashboard show only the scoped workspace,
// switching to another workspace and creating one are 403; password sessions unchanged.
func TestWorkspacesSSOScopedSession(t *testing.T) {
	e := newWsEnv(t)
	uid, pwd := e.user(t, "t11-ssos")
	n := uniq(t)
	a, b := n+"-a", n+"-b"
	e.mk(t, a, "Alpha", a, uid, 1788000000, map[int32]string{uid: "member"})
	e.mk(t, b, "Beta", b, uid, 1788000001, map[int32]string{uid: "member"})
	e.addLeads(t, a, 2)
	e.addLeads(t, b, 3)
	e.setActive(t, uid, b)
	sso := ssoBearer(t, "t11-ssos", a)

	rec, _ := e.do("GET", "/api/workspaces", "", "", sso)
	m := decodeMap(t, rec.Body.Bytes())
	ws, _ := m["workspaces"].([]any)
	if rec.Code != 200 || len(ws) != 1 || ws[0].(map[string]any)["id"] != a || ws[0].(map[string]any)["is_active"] != true || m["active_id"] != a {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = e.do("GET", "/api/workspaces/dashboard", "", "", sso)
	m = decodeMap(t, rec.Body.Bytes())
	if rec.Code != 200 || m["total_workspaces"] != float64(1) || m["total_leads"] != float64(2) {
		t.Fatalf("dashboard %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = e.do("POST", "/api/workspaces/switch", "application/json", mustJSON(map[string]string{"workspace_id": b}), sso)
	if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Workspace access denied"}` || e.active(t, uid) != b {
		t.Fatalf("switch foreign %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ = e.do("POST", "/api/workspaces/switch", "application/json", mustJSON(map[string]string{"workspace_id": a}), sso); rec.Code != 200 || e.active(t, uid) != a {
		t.Fatalf("switch own %d %s", rec.Code, rec.Body.String())
	}
	slug := n + "-new"
	rec, _ = e.do("POST", "/api/workspaces", "application/json", mustJSON(map[string]string{"name": slug}), sso)
	if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Workspace access denied"}` {
		t.Fatalf("create %d %s", rec.Code, rec.Body.String())
	}
	var cnt int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM workspaces WHERE slug=$1`, slug).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("workspace created: %d %v", cnt, err)
	}

	rec, _ = e.do("GET", "/api/workspaces", "", "", pwd)
	if ws, _ := decodeMap(t, rec.Body.Bytes())["workspaces"].([]any); rec.Code != 200 || len(ws) < 2 {
		t.Fatalf("password list %d %s", rec.Code, rec.Body.String())
	}
}

// Workbook WebSocket: an SSO session connects to its own workspace (no regression) and is refused for
// another workspace the user belongs to.
func TestWSSSOScopedToken(t *testing.T) {
	e := newWBSockEnv(t, nil)
	tok, _ := testSigner.AccessFor(wsTag+"bob", []string{"sso"}, e.wsA)
	c, _, err := e.dial(e.wb1, "token="+tok+"&workspace_id="+e.wsA)
	if err != nil {
		t.Fatalf("own workspace: %v", err)
	}
	_ = c.CloseNow()
	_, resp, err := e.dial(e.wbB, "token="+tok+"&workspace_id="+e.wsB)
	if err == nil || handshakeStatus(resp) != 403 {
		t.Fatalf("foreign workspace: err=%v status=%d", err, handshakeStatus(resp))
	}
	e.mustDial(t, e.wbB, "bob", e.wsB) // bob's password session still reaches beta
}

// Legacy ?token= SSE: an SSO session streams its own workspace, is 403 for another, and without
// workspace_id uses the scope (not the active workspace).
func TestEventsQueryTokenSSOScoped(t *testing.T) {
	setHeartbeat(t, 50*time.Millisecond)
	e := newEventsEnv(t, nil)
	tag := fmt.Sprintf("t16s%d", time.Now().UnixNano()%1e9)
	ws1 := e.seedWS(t, tag)
	alice := e.mustUserByName(t, tag+"-alice")
	ws2 := tag + "-ws2"
	e.workspace(t, ws2, alice.ID)
	e.member(t, ws2, alice.ID, "owner")
	tok, _ := testSigner.AccessFor(tag+"-alice", []string{"sso"}, ws1)
	if resp, cancel, _ := openStream(t, e, "/api/events?token="+tok+"&workspace_id="+ws1, nil); resp.StatusCode != 200 {
		t.Fatalf("own: %d", resp.StatusCode)
	} else {
		cancel()
	}
	if resp, _, _ := openStream(t, e, "/api/events?token="+tok+"&workspace_id="+ws2, nil); resp.StatusCode != 403 {
		t.Fatalf("foreign: %d", resp.StatusCode)
	}
	// No workspace_id: the scope (ws1) is used. Remove alice from ws1 and point her active
	// workspace at ws2: a scope-respecting stream is refused, the active fallback would succeed.
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM user_active_workspace WHERE user_id=$1`, alice.ID)
	})
	exec(`DELETE FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, ws1, alice.ID)
	exec(`UPDATE workspaces SET owner_id=NULL WHERE id=$1`, ws1)
	exec(`INSERT INTO user_active_workspace (user_id, workspace_id) VALUES ($1,$2) ON CONFLICT (user_id) DO UPDATE SET workspace_id=EXCLUDED.workspace_id`, alice.ID, ws2)
	if resp, _, _ := openStream(t, e, "/api/events?token="+tok, nil); resp.StatusCode != 403 {
		t.Fatalf("no workspace_id: %d", resp.StatusCode)
	}
	pwd := e.tokenQuery(t, tag+"-alice", []string{"pwd"})
	if resp, _, _ := openStream(t, e, "/api/events?token="+pwd, nil); resp.StatusCode != 200 {
		t.Fatalf("password active fallback: %d", resp.StatusCode)
	}
}
