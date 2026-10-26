//go:build integration

package outreachstore_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func lockRow(t *testing.T, ctx context.Context, tx pgx.Tx, email string) (reason, source string, locked bool) {
	t.Helper()
	if err := tx.QueryRow(ctx, `SELECT reason, source, locked FROM outreach_suppressions WHERE email=$1`, email).Scan(&reason, &source, &locked); err != nil {
		t.Fatalf("%s: %v", email, err)
	}
	return
}

// An explicit opt-out (locked=true) upgrades an existing unlocked row; a plain add never changes one; a locked row is never downgraded
// and keeps its reason.
func TestAddSuppressionLockUpgrade(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := supNewWS(t, pool)
	supTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		add := func(email, reason, source string, locked bool) bool {
			added, err := outreachstore.AddSuppression(ctx, tx, ws, email, reason, source, locked)
			if err != nil {
				t.Fatal(err)
			}
			return added
		}
		if !add("m@example.com", "manual", "manual", false) || !add("b@example.com", "bounce", "seq", false) {
			t.Fatal("seed rows not added")
		}
		// unlocked over unlocked: untouched
		if add("m@example.com", "bounce", "x", false) {
			t.Fatal("duplicate reported as added")
		}
		if r, s, l := lockRow(t, ctx, tx, "m@example.com"); r != "manual" || s != "manual" || l {
			t.Fatalf("%s %s %v", r, s, l)
		}
		// unsubscribe over manual: upgraded, still added=false
		if add("M@Example.com", "unsubscribe", "seq-1", true) {
			t.Fatal("an upgrade is not an add")
		}
		if r, s, l := lockRow(t, ctx, tx, "m@example.com"); r != "unsubscribe" || s != "seq-1" || !l {
			t.Fatalf("%s %s %v", r, s, l)
		}
		// complaint over bounce: locked complaint
		add("b@example.com", "complaint", "webhook", true)
		if r, s, l := lockRow(t, ctx, tx, "b@example.com"); r != "complaint" || s != "webhook" || !l {
			t.Fatalf("%s %s %v", r, s, l)
		}
		// complaint over a locked unsubscribe keeps the unsubscribe; manual add after lock stays locked
		add("m@example.com", "complaint", "webhook", true)
		add("m@example.com", "manual", "manual", false)
		if r, s, l := lockRow(t, ctx, tx, "m@example.com"); r != "unsubscribe" || s != "seq-1" || !l {
			t.Fatalf("%s %s %v", r, s, l)
		}
		// a legacy dotted gmail row is upgraded through the match keys; no second row appears
		if _, err := tx.Exec(ctx, `INSERT INTO outreach_suppressions (workspace_id, email, reason, source, locked) VALUES ($1,'l.e.g@gmail.com','manual','manual',false)`, ws); err != nil {
			t.Fatal(err)
		}
		add("leg+x@gmail.com", "unsubscribe", "seq-2", true)
		if r, _, l := lockRow(t, ctx, tx, "l.e.g@gmail.com"); r != "unsubscribe" || !l {
			t.Fatalf("%s %v", r, l)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM outreach_suppressions WHERE workspace_id=$1 AND email LIKE 'leg%'`, ws).Scan(&n); err != nil || n != 0 {
			t.Fatalf("extra row: %d %v", n, err)
		}
	})
}
