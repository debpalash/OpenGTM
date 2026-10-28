package testpg

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// reserveGap separates a reserved block from the sequence value ReserveIDs drew: a default-id insert by another
// package that calls nextval between ReserveIDs' nextval and its setval takes one of the gap ids, never a block id.
const reserveGap = 1000

// ReserveIDs reserves n consecutive ids of table's serial id column for the caller and returns base: the ids
// base+1 .. base+n are the caller's alone. Test packages run in parallel against one database (go test -p 4 ./...),
// so a seed that inserts explicit ids must take them from a reserved block: a hard-coded id collides with rows another
// package created (or creates later) from the sequence, and moving the sequence (setval to a fixed value, or to max(id)
// while another package's insert is uncommitted) hands ids that already exist to the next default-id insert anywhere.
// Recorded fixtures keep their ids meaningful by mapping recorded id r to base+r.
//
// The sequence only ever moves forward: past both its own value and the table's max(id), in one transaction
// serialized across callers by an advisory lock. Reads TEST_DATABASE_URL (owner credentials; the app role cannot
// setval) and skips the test when it is unset.
func ReserveIDs(t testing.TB, table string, n int64) int64 {
	t.Helper()
	if n < 1 {
		t.Fatalf("ReserveIDs(%s): n=%d", table, n)
	}
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("ReserveIDs(%s): connect: %v", table, err)
	}
	defer conn.Close(ctx)
	var last int64
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7770002)`); err != nil {
			return err
		}
		var seq *string
		if err := tx.QueryRow(ctx, `SELECT pg_get_serial_sequence($1, 'id')`, table).Scan(&seq); err != nil {
			return err
		}
		if seq == nil {
			t.Fatalf("ReserveIDs: %s has no serial id", table)
		}
		return tx.QueryRow(ctx, `SELECT setval($1::regclass, GREATEST(nextval($1::regclass), (SELECT coalesce(max(id), 0) FROM `+
			pgx.Identifier{table}.Sanitize()+`)) + $2)`, *seq, int64(reserveGap)+n).Scan(&last)
	})
	if err != nil {
		t.Fatalf("ReserveIDs(%s): %v", table, err)
	}
	return last - n
}
