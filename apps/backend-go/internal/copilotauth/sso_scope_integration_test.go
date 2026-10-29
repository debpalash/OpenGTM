//go:build integration

package copilotauth

import "testing"

// An SSO session (sso_ws) resolves to its own workspace only: a foreign X-Workspace-Id is refused even
// for a member, and without the header the scope wins over the stored active workspace.
func TestSSOScopedTokenStaysInItsWorkspace(t *testing.T) {
	e := newEnv(t)
	u := e.user(t, "t10s-multi", true)
	e.workspace(t, "t10s-ws-1", "t10s-slug-1", nil)
	e.workspace(t, "t10s-ws-2", "t10s-slug-2", nil)
	e.member(t, "t10s-ws-1", u)
	e.member(t, "t10s-ws-2", u)
	e.active(t, "t10s-ws-2", u)
	tok, _ := testSigner.AccessFor("t10s-multi", []string{"sso"}, "t10s-ws-1")
	scoped := func(ws string) (Resolved, *Err) {
		r := req("Bearer " + tok)
		if ws != "" {
			r.Header.Set("X-Workspace-Id", ws)
		}
		return Resolve(r, e.deps(true))
	}
	_, err := scoped("t10s-ws-2")
	wantErr(t, err, 403, "Workspace access denied")
	if got, err := scoped(""); err != nil || got.WorkspaceID != "t10s-ws-1" {
		t.Fatalf("no header: %+v %v", got, err)
	}
	if got, err := scoped("t10s-ws-1"); err != nil || got.WorkspaceID != "t10s-ws-1" {
		t.Fatalf("own: %+v %v", got, err)
	}
	// password tokens unchanged
	if got, err := Resolve(e.request("t10s-multi", "t10s-ws-2"), e.deps(true)); err != nil || got.WorkspaceID != "t10s-ws-2" {
		t.Fatalf("password: %+v %v", got, err)
	}
}
