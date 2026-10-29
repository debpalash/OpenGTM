//go:build integration

package govadmin

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// S-L21: a delegated manager (editor with a governance.manage allow) may not remove a peer
// that carries a deny override: remove and re-add would wipe an admin-set deny. An admin
// still may, and a peer without a deny can still be removed by the delegated manager.
func TestCheckRemoveDelegatedManagerCannotWipeDeny(t *testing.T) {
	e := setup(t)
	peer := mkUser(t, e.pool, "slr21-peer")
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, peer) })
	exec(t, e.pool, `INSERT INTO workspace_members (workspace_id,user_id,role,created_at) VALUES ($1,$2,'editor',4)`, e.ws, peer)
	if _, err := setOv(t, e, e.ws, e.editor, "governance.manage", sp("allow")); err != nil {
		t.Fatal(err)
	}
	check := func(actor int32) error {
		return e.in(t, e.ws, func(tx pgx.Tx) error { return CheckRemove(context.Background(), tx, e.ws, actor, peer) })
	}
	if err := check(e.editor); err != nil {
		t.Fatalf("peer without deny: %v", err)
	}
	if _, err := setOv(t, e, e.ws, peer, "outreach.write", sp("deny")); err != nil {
		t.Fatal(err)
	}
	if err := check(e.editor); !errors.Is(err, ErrEscalation) {
		t.Fatalf("delegated manager removing a denied peer: %v", err)
	}
	if err := check(e.admin); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if err := check(e.owner); err != nil {
		t.Fatalf("owner: %v", err)
	}
}
