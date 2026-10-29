//go:build integration

package api

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// TestRuntimeRoleCanReadWriteEmailData: the runtime (app login) role holds SELECT, INSERT,
// UPDATE and DELETE on the legacy global tables email_data and links, and USAGE on their
// id sequences, and can run a full write cycle (rolled back).
func TestRuntimeRoleCanReadWriteEmailData(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	ctx := context.Background()
	for _, tbl := range []string{"email_data", "links"} {
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, "public."+tbl, priv).Scan(&ok); err != nil {
				t.Fatalf("%s %s: %v", tbl, priv, err)
			}
			if !ok {
				t.Errorf("runtime role lacks %s on %s", priv, tbl)
			}
		}
		var seq *string
		if err := pool.QueryRow(ctx, `SELECT pg_get_serial_sequence($1, 'id')`, "public."+tbl).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if seq == nil {
			t.Fatalf("%s.id has no sequence", tbl)
		}
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_sequence_privilege(current_user, $1, 'USAGE')`, *seq).Scan(&ok); err != nil || !ok {
			t.Errorf("runtime role lacks USAGE on %s (%v)", *seq, err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var linkID, id int
	if err := tx.QueryRow(ctx, `INSERT INTO links (url) VALUES ('https://role-check.example') RETURNING id`).Scan(&linkID); err != nil {
		t.Fatalf("insert links: %v", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO email_data (name, email, source_link_id) VALUES ('role', 'role-check@example.com', $1) RETURNING id`, linkID).Scan(&id); err != nil {
		t.Fatalf("insert email_data: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE email_data SET is_used = true WHERE id = $1`, id); err != nil {
		t.Fatalf("update email_data: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM email_data WHERE id = $1 AND is_used`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("select email_data: n=%d err=%v", n, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM email_data WHERE id = $1`, id); err != nil {
		t.Fatalf("delete email_data: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM links WHERE id = $1`, linkID); err != nil {
		t.Fatalf("delete links: %v", err)
	}
}
