//go:build integration

package govadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool    *pgxpool.Pool
	ws, ws2 string
	owner   int32
	admin   int32
	editor  int32
}

func exec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mkUser(t *testing.T, p *pgxpool.Pool, name string) int32 {
	t.Helper()
	var id int32
	if err := p.QueryRow(context.Background(),
		`INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ($1,'x',true,false,'user') RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func setup(t *testing.T) *env {
	t.Helper()
	p := testpg.NewAppPool(t, 4)
	tag := fmt.Sprintf("ga%d", testCounter())
	e := &env{pool: p, ws: "ws-" + tag, ws2: "ws-" + tag + "-b"}
	e.owner = mkUser(t, p, tag+"-owner")
	e.admin = mkUser(t, p, tag+"-admin")
	e.editor = mkUser(t, p, tag+"-editor")
	for _, w := range []string{e.ws, e.ws2} {
		exec(t, p, `INSERT INTO workspaces (id,name,slug,description,icon,owner_id,created_at,updated_at) VALUES ($1::text,$1::text,$1::text,'','',$2,1,1)`, w, e.owner)
		exec(t, p, `INSERT INTO workspace_members (workspace_id,user_id,role,created_at) VALUES ($1,$2,'owner',1)`, w, e.owner)
	}
	exec(t, p, `INSERT INTO workspace_members (workspace_id,user_id,role,created_at) VALUES ($1,$2,'admin',2),($1,$3,'editor',3)`, e.ws, e.admin, e.editor)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, w := range []string{e.ws, e.ws2} {
			_ = tenancy.InWorkspace(ctx, p, w, func(tx pgx.Tx) error {
				for _, q := range []string{
					`DELETE FROM workspace_scim_group_members`, `DELETE FROM workspace_scim_groups`,
					`DELETE FROM workspace_oidc_identities`, `DELETE FROM workspace_member_permissions`,
					`DELETE FROM workspace_members`} {
					_, _ = tx.Exec(ctx, q+` WHERE workspace_id=$1`, w)
				}
				return nil
			})
			_, _ = p.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, w)
		}
		_, _ = p.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, []int32{e.owner, e.admin, e.editor})
	})
	return e
}

var ctr int

func testCounter() int { ctr++; return ctr*1000 + os.Getpid()%1000 }

func (e *env) in(t *testing.T, ws string, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return tenancy.InWorkspace(context.Background(), e.pool, ws, fn)
}

func TestOverviewUsernameFallback(t *testing.T) {
	e := setup(t)
	// A member whose users row is gone (FK-less): fallback "User <id>".
	exec(t, e.pool, `INSERT INTO workspace_members (workspace_id,user_id,role,created_at) VALUES ($1,987654,'viewer',4)`, e.ws)
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM workspace_members WHERE user_id=987654`) })
	var r RBAC
	if err := e.in(t, e.ws, func(tx pgx.Tx) error {
		var err error
		r, err = Overview(context.Background(), tx, e.ws)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(r.Permissions) != 11 || len(r.Members) != 4 {
		t.Fatalf("shape %d/%d", len(r.Permissions), len(r.Members))
	}
	if r.Members[0].Role != "owner" || r.Members[1].Role != "admin" || r.Members[3].Username != "User 987654" {
		t.Fatalf("order/fallback: %+v", r.Members)
	}
	b, _ := json.Marshal(r.Members[3])
	if string(b) != `{"user_id":987654,"role":"viewer","created_at":4,"username":"User 987654","overrides":{}}` {
		t.Fatalf("json: %s", b)
	}
}

func setOv(t *testing.T, e *env, ws string, uid int32, perm string, eff *string) (Effective, error) {
	var out Effective
	err := e.in(t, ws, func(tx pgx.Tx) error {
		var err error
		out, err = SetOverride(context.Background(), tx, ws, uid, perm, eff)
		return err
	})
	return out, err
}

func sp(s string) *string { return &s }

func TestSetOverrideEffectiveComputation(t *testing.T) {
	e := setup(t)
	cases := []struct {
		perm string
		eff  *string
		want bool
	}{
		{"tables.write", sp("allow"), true},
		{"tables.write", sp("deny"), false},
		{"tables.write", nil, true},    // editor in defaults
		{"outreach.write", nil, false}, // editor not in defaults
		{"outreach.write", sp("allow"), true},
	}
	for _, c := range cases {
		got, err := setOv(t, e, e.ws, e.editor, c.perm, c.eff)
		if err != nil || got.Effective != c.want || got.UserID != e.editor || got.Permission != c.perm {
			t.Fatalf("%v: %+v %v", c, got, err)
		}
	}
	// null removed the override row.
	var r RBAC
	_ = e.in(t, e.ws, func(tx pgx.Tx) (err error) { r, err = Overview(context.Background(), tx, e.ws); return })
	for _, m := range r.Members {
		if m.UserID == e.editor && (len(m.Overrides) != 1 || m.Overrides["outreach.write"] != "allow") {
			t.Fatalf("overrides %v", m.Overrides)
		}
	}
	b, _ := json.Marshal(Effective{UserID: 1, Permission: "p", Effect: sp("allow"), Effective: true})
	if string(b) != `{"user_id":1,"permission":"p","effect":"allow","effective":true}` {
		t.Fatal(string(b))
	}
}

func TestSetOverrideOwnerAndNonMember(t *testing.T) {
	e := setup(t)
	if _, err := setOv(t, e, e.ws, e.owner, "tables.write", sp("deny")); !errors.Is(err, ErrOwner) {
		t.Fatalf("owner: %v", err)
	}
	if _, err := setOv(t, e, e.ws, 424242, "tables.write", sp("deny")); !errors.Is(err, ErrNotMember) {
		t.Fatalf("nonmember: %v", err)
	}
	if _, err := setOv(t, e, e.ws, e.editor, "nope.write", sp("deny")); !errors.Is(err, ErrUnknownPermission) {
		t.Fatalf("unknown: %v", err)
	}
	// Unknown permission wins over member checks (Python order).
	if _, err := setOv(t, e, e.ws, 424242, "nope.write", nil); !errors.Is(err, ErrUnknownPermission) {
		t.Fatalf("order: %v", err)
	}
	// Invalid effect is rejected, never stored.
	if _, err := setOv(t, e, e.ws, e.editor, "tables.write", sp("maybe")); err == nil {
		t.Fatal("invalid effect accepted")
	}
	// Admin of ws2? editor is not a member of ws2.
	if _, err := setOv(t, e, e.ws2, e.editor, "tables.write", sp("deny")); !errors.Is(err, ErrNotMember) {
		t.Fatalf("cross ws: %v", err)
	}
}

func addMember(t *testing.T, e *env, ws, name, role string) (MemberOut, error) {
	var out MemberOut
	err := e.in(t, ws, func(tx pgx.Tx) error {
		var err error
		out, err = AddMember(context.Background(), tx, ws, name, role)
		return err
	})
	return out, err
}

func TestAddMemberDuplicateAndUnknown(t *testing.T) {
	e := setup(t)
	nu := mkUser(t, e.pool, fmt.Sprintf("newbie%d", e.owner))
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM workspace_members WHERE user_id=$1`, nu)
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, nu)
	})
	if _, err := addMember(t, e, e.ws, "ghost-no-such", "viewer"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	out, err := addMember(t, e, e.ws, fmt.Sprintf("  newbie%d ", e.owner), "editor")
	if err != nil || out.UserID != nu || out.Role != "editor" || out.Overrides == nil || len(out.Overrides) != 0 {
		t.Fatalf("add: %+v %v", out, err)
	}
	b, _ := json.Marshal(out)
	if string(b) != fmt.Sprintf(`{"user_id":%d,"username":"newbie%d","role":"editor","overrides":{}}`, nu, e.owner) {
		t.Fatal(string(b))
	}
	if _, err := addMember(t, e, e.ws, fmt.Sprintf("newbie%d", e.owner), "viewer"); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("dup: %v", err)
	}
	// Workspace owner without a member row still counts as a member.
	exec(t, e.pool, `DELETE FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, e.ws2, e.owner)
	var uname string
	_ = e.pool.QueryRow(context.Background(), `SELECT username FROM users WHERE id=$1`, e.owner).Scan(&uname)
	if _, err := addMember(t, e, e.ws2, uname, "viewer"); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("owner w/o row: %v", err)
	}
}

func TestAddMemberOwnerRoleNotAssignable(t *testing.T) {
	e := setup(t)
	nu := mkUser(t, e.pool, fmt.Sprintf("nr%d", e.owner))
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, nu) })
	for _, role := range []string{"owner", "", "root", "Admin"} {
		if _, err := addMember(t, e, e.ws, fmt.Sprintf("nr%d", e.owner), role); !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("role %q: %v", role, err)
		}
	}
	for _, role := range []string{"owner", "superadmin"} {
		err := e.in(t, e.ws, func(tx pgx.Tx) error { _, err := SetRole(context.Background(), tx, e.ws, e.editor, role); return err })
		if !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("setrole %q: %v", role, err)
		}
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_members WHERE user_id=$1`, nu).Scan(&n)
	if n != 0 {
		t.Fatal("member row created for invalid role")
	}
}

func TestSetRoleKeepsOverrides(t *testing.T) {
	e := setup(t)
	if _, err := setOv(t, e, e.ws, e.editor, "outreach.write", sp("allow")); err != nil {
		t.Fatal(err)
	}
	var out MemberOut
	err := e.in(t, e.ws, func(tx pgx.Tx) (err error) {
		out, err = SetRole(context.Background(), tx, e.ws, e.editor, "viewer")
		return
	})
	if err != nil || out.Role != "viewer" || out.Overrides["outreach.write"] != "allow" {
		t.Fatalf("%+v %v", out, err)
	}
	b, _ := json.Marshal(out)
	if string(b) != fmt.Sprintf(`{"user_id":%d,"role":"viewer","overrides":{"outreach.write":"allow"}}`, e.editor) {
		t.Fatal(string(b))
	}
	// Owner role immutable; non-member 404; self-demotion by admin is allowed (Python parity).
	for _, c := range []struct {
		uid  int32
		want error
	}{{e.owner, ErrOwnerRole}, {424242, ErrNotMember}} {
		err := e.in(t, e.ws, func(tx pgx.Tx) error { _, err := SetRole(context.Background(), tx, e.ws, c.uid, "viewer"); return err })
		if !errors.Is(err, c.want) {
			t.Fatalf("%d: %v", c.uid, err)
		}
	}
	if err := e.in(t, e.ws, func(tx pgx.Tx) error {
		_, err := SetRole(context.Background(), tx, e.ws, e.admin, "viewer")
		return err
	}); err != nil {
		t.Fatalf("self-demote: %v", err)
	}
}

func TestRemoveCleansOIDCOverridesAndSCIMGroups(t *testing.T) {
	e := setup(t)
	if _, err := setOv(t, e, e.ws, e.editor, "outreach.write", sp("allow")); err != nil {
		t.Fatal(err)
	}
	_ = e.in(t, e.ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO workspace_oidc_identities (workspace_id,issuer,subject,user_id,created_at) VALUES ($1,'iss','sub-1',$2,1)`, e.ws, e.editor)
		if err != nil {
			t.Fatal(err)
		}
		return nil
	})
	for _, w := range []string{e.ws} {
		_ = e.in(t, w, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO workspace_scim_groups (id,workspace_id,external_id,display_name,created_at,updated_at) VALUES ('g-`+e.ws+`',$1,'','G',1,1)`, w)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(context.Background(), `INSERT INTO workspace_scim_group_members (workspace_id,group_id,user_id,created_at) VALUES ($1,'g-`+e.ws+`',$2,1),($1,'g-`+e.ws+`',$3,1)`, w, e.editor, e.admin)
			if err != nil {
				t.Fatal(err)
			}
			return nil
		})
	}
	rm := func(ws string, uid int32) error {
		return e.in(t, ws, func(tx pgx.Tx) error { return Remove(context.Background(), tx, ws, uid) })
	}
	if err := rm(e.ws, e.owner); !errors.Is(err, ErrOwnerRemove) {
		t.Fatalf("owner: %v", err)
	}
	if err := rm(e.ws, 424242); !errors.Is(err, ErrNotMember) {
		t.Fatalf("nonmember: %v", err)
	}
	if err := rm(e.ws, e.editor); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) (n int) {
		_ = e.in(t, e.ws, func(tx pgx.Tx) error { return tx.QueryRow(context.Background(), q, args...).Scan(&n) })
		return
	}
	if n := count(`SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, e.ws, e.editor); n != 0 {
		t.Fatal("member row remains")
	}
	if n := count(`SELECT count(*) FROM workspace_member_permissions WHERE user_id=$1`, e.editor); n != 0 {
		t.Fatal("overrides remain")
	}
	if n := count(`SELECT count(*) FROM workspace_oidc_identities WHERE user_id=$1 AND workspace_id=$2`, e.editor, e.ws); n != 0 {
		t.Fatal("oidc remains")
	}
	if n := count(`SELECT count(*) FROM workspace_scim_group_members WHERE user_id=$1`, e.editor); n != 0 {
		t.Fatal("scim group membership remains")
	}
	if n := count(`SELECT count(*) FROM workspace_scim_group_members WHERE user_id=$1`, e.admin); n != 1 {
		t.Fatal("other member's group membership removed")
	}
}

func TestMembersTenantIsolation(t *testing.T) {
	e := setup(t)
	// ws2 only has the owner. Operating on ws members through ws2 context is 404.
	if _, err := setOv(t, e, e.ws2, e.admin, "tables.write", sp("deny")); !errors.Is(err, ErrNotMember) {
		t.Fatalf("override: %v", err)
	}
	err := e.in(t, e.ws2, func(tx pgx.Tx) error {
		_, err := SetRole(context.Background(), tx, e.ws2, e.admin, "viewer")
		return err
	})
	if !errors.Is(err, ErrNotMember) {
		t.Fatalf("role: %v", err)
	}
	if err := e.in(t, e.ws2, func(tx pgx.Tx) error { return Remove(context.Background(), tx, e.ws2, e.admin) }); !errors.Is(err, ErrNotMember) {
		t.Fatalf("remove: %v", err)
	}
	var r RBAC
	_ = e.in(t, e.ws2, func(tx pgx.Tx) (err error) { r, err = Overview(context.Background(), tx, e.ws2); return })
	if len(r.Members) != 1 {
		t.Fatalf("leak: %+v", r.Members)
	}
	// ws unchanged.
	_ = e.in(t, e.ws, func(tx pgx.Tx) (err error) { r, err = Overview(context.Background(), tx, e.ws); return })
	if len(r.Members) != 3 {
		t.Fatalf("ws members %d", len(r.Members))
	}
}
