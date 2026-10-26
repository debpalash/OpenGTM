//go:build integration

package identity

import (
	"context"
	"testing"
)

func TestStoreAccessHelpers(t *testing.T) {
	e := newWSEnv(t, 4)
	ctx := context.Background()
	owner, editor, viewer, out := e.user(t, "acc-owner"), e.user(t, "acc-editor"), e.user(t, "acc-viewer"), e.user(t, "acc-out")
	e.workspace(t, "acc-ws1", "acc-ws1", &owner)
	e.workspace(t, "acc-ws2", "acc-ws2", nil)
	e.member(t, "acc-ws1", editor, "editor")
	e.member(t, "acc-ws1", viewer, "viewer")
	e.member(t, "acc-ws2", editor, "viewer")

	must := func(b bool, err error) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// Owner without a member row is a member (workspaces.owner_id).
	if !must(e.st.IsMember(ctx, "acc-ws1", owner.ID)) || !must(e.st.IsMember(ctx, "acc-ws1", viewer.ID)) {
		t.Fatal("members rejected")
	}
	if must(e.st.IsMember(ctx, "acc-ws1", out.ID)) || must(e.st.IsMember(ctx, "nope", owner.ID)) {
		t.Fatal("non-member accepted")
	}
	// Permissions: default roles, owner role needs a member row, explicit override wins.
	const p = "integrations.manage"
	if !must(e.st.HasPermission(ctx, "acc-ws1", editor.ID, p, "admin", "editor")) {
		t.Fatal("editor default denied")
	}
	if must(e.st.HasPermission(ctx, "acc-ws1", viewer.ID, p, "admin", "editor")) {
		t.Fatal("viewer default allowed")
	}
	if must(e.st.HasPermission(ctx, "acc-ws1", owner.ID, p, "admin", "editor")) {
		t.Fatal("owner_id without a member row has no role (Python member_role is None)")
	}
	e.member(t, "acc-ws1", out, "owner")
	if !must(e.st.HasPermission(ctx, "acc-ws1", out.ID, p, "admin")) {
		t.Fatal("owner role must pass")
	}
	e.perm(t, "acc-ws1", editor, p, "deny")
	e.perm(t, "acc-ws1", viewer, p, "allow")
	if must(e.st.HasPermission(ctx, "acc-ws1", editor.ID, p, "admin", "editor")) {
		t.Fatal("deny override ignored")
	}
	if !must(e.st.HasPermission(ctx, "acc-ws1", viewer.ID, p, "admin", "editor")) {
		t.Fatal("allow override ignored")
	}
	// Active workspace: stored one while still a member, else first accessible, else "".
	got := func(u User) string {
		s, err := e.st.ActiveWorkspaceID(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	e.active(t, "acc-ws2", editor)
	if got(editor) != "acc-ws2" {
		t.Fatalf("active %q", got(editor))
	}
	e.active(t, "acc-ws2", viewer) // no longer a member of ws2
	if got(viewer) != "acc-ws1" {
		t.Fatalf("fallback %q", got(viewer))
	}
	lonely := e.user(t, "acc-lonely")
	if got(lonely) != "" {
		t.Fatalf("lonely %q", got(lonely))
	}
}
