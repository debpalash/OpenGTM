//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func rows(t *testing.T, pool *pgxpool.Pool, ws string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
		rs, err := tx.Query(context.Background(), `SELECT email, reason||'/'||source||'/'||locked FROM outreach_suppressions WHERE workspace_id=$1`, ws)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var e, v string
			if err := rs.Scan(&e, &v); err != nil {
				return err
			}
			out[e] = v
		}
		return rs.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNormalizeRekeysLegacyGmailRows(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	ws := "supn-" + hex.EncodeToString(b)
	ctx := context.Background()
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM outreach_suppressions WHERE workspace_id=$1`, ws)
			return err
		})
	})
	seed := [][]any{
		{"p.a.t.lee+news@gmail.com", "unsubscribe", "seq", true}, // dotted+plus, no collapsed twin: re-keyed
		{"j.o.e@googlemail.com", "bounce", "imap", false},        // dotted unlocked, twin exists: merged, twin unchanged
		{"joe@googlemail.com", "manual", "manual", false},        //
		{"d.o.t@gmail.com", "complaint", "inbound", true},        // dotted locked, unlocked twin: merged, the lock is carried over
		{"dot@gmail.com", "manual", "manual", false},             //
		{"a.b@gmail.com", "manual", "manual", false},             // dotted, locked twin: merged, twin stays locked
		{"ab@gmail.com", "complaint", "inbound", true},           //
		{"plain@gmail.com", "manual", "manual", false},           // already collapsed: untouched
		{"first.last@example.com", "manual", "manual", false},    // not gmail: untouched
	}
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		for _, r := range seed {
			if _, err := tx.Exec(ctx, `INSERT INTO outreach_suppressions (workspace_id, email, reason, source, locked) VALUES ($1,$2,$3,$4,$5)`, append([]any{ws}, r...)...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// dry run changes nothing
	st, err := Normalize(ctx, pool, Options{Workspace: ws, BatchSize: 2, DryRun: true})
	if err != nil || st.Rekeyed != 1 || st.Merged != 3 || len(rows(t, pool, ws)) != len(seed) {
		t.Fatalf("dry run: %+v %v", st, err)
	}
	for run := 0; run < 2; run++ { // the second run is a no-op
		st, err = Normalize(ctx, pool, Options{Workspace: ws, BatchSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 && (st.Rekeyed != 1 || st.Merged != 3) || run == 1 && (st.Rekeyed != 0 || st.Merged != 0) {
			t.Fatalf("run %d: %+v", run, st)
		}
	}
	want := map[string]string{
		"patlee@gmail.com":       "unsubscribe/seq/true",
		"joe@googlemail.com":     "manual/manual/false",
		"dot@gmail.com":          "complaint/inbound/true",
		"ab@gmail.com":           "complaint/inbound/true",
		"plain@gmail.com":        "manual/manual/false",
		"first.last@example.com": "manual/manual/false",
	}
	got := rows(t, pool, ws)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows\n got %v\nwant %v", got, want)
	}
}
