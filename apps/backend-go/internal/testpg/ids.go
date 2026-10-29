package testpg

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// SeqFloor is the lowest value any serial sequence hands out under the test helpers: NewAppPool and ReserveIDs raise
// every public sequence to at least SeqFloor (never lower one). Ids below it belong to recorded fixtures whose literal
// ids are part of the recording (request paths, response bodies) and cannot move; a sequence-generated or reserved id
// never lands on one of them. Every other explicit-id seed takes its ids from ReserveIDs.
const SeqFloor = 10_000_000

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
// The sequence only ever moves forward: past its own value, the table's max(id) and SeqFloor, in one transaction
// serialized across callers (and raiseSeqFloor) by an advisory lock. Reads TEST_DATABASE_URL (owner credentials; the app role cannot
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
			pgx.Identifier{table}.Sanitize()+`), $3) + $2)`, *seq, int64(reserveGap)+n, int64(SeqFloor)).Scan(&last)
	})
	if err != nil {
		t.Fatalf("ReserveIDs(%s): %v", table, err)
	}
	return last - n
}

// raiseSeqFloor moves every public sequence still below SeqFloor up to it. It holds the same advisory lock as
// ReserveIDs, so the read-then-set never lowers a sequence another caller raised in between (a plain nextval in the
// window only returns a value below SeqFloor, which is still free in sequence space).
func raiseSeqFloor(ctx context.Context, owner interface {
	Begin(context.Context) (pgx.Tx, error)
}) error {
	return pgx.BeginFunc(ctx, owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7770002)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT setval(format('%I.%I', schemaname, sequencename)::regclass, $1)
			FROM pg_sequences WHERE schemaname = 'public' AND coalesce(last_value, 0) < $1 AND max_value > $1`, int64(SeqFloor))
		return err
	})
}

// FreeIDs prepares literal ids a recorded fixture must insert (they appear in its recorded requests or responses and
// cannot be remapped): each must be below SeqFloor, where no sequence and no ReserveIDs block ever lands, and a row
// another test left on one of them is deleted (owner connection, across workspaces). Reads TEST_DATABASE_URL.
func FreeIDs(t testing.TB, table string, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if id >= SeqFloor {
			t.Fatalf("FreeIDs(%s): id %d is not below SeqFloor; take it from ReserveIDs", table, id)
		}
	}
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("FreeIDs(%s): connect: %v", table, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DELETE FROM `+pgx.Identifier{table}.Sanitize()+` WHERE id = ANY($1)`, ids); err != nil {
		t.Fatalf("FreeIDs(%s): %v", table, err)
	}
}

// Span returns the ids from..to inclusive, for FreeIDs.
func Span(from, to int64) []int64 {
	out := make([]int64, 0, to-from+1)
	for id := from; id <= to; id++ {
		out = append(out, id)
	}
	return out
}

// FreeRecordIDs is FreeIDs for recorded seed rows (JSON objects): the numeric "id" of each row is freed. Rows without
// one, or with a text id (no sequence behind it), are skipped.
func FreeRecordIDs(t testing.TB, table string, rows ...json.RawMessage) {
	t.Helper()
	var ids []int64
	for _, r := range rows {
		var v struct {
			ID any `json:"id"`
		}
		if err := json.Unmarshal(r, &v); err != nil {
			t.Fatalf("FreeRecordIDs(%s): %v", table, err)
		}
		if n, ok := v.ID.(float64); ok {
			ids = append(ids, int64(n))
		}
	}
	if len(ids) > 0 {
		FreeIDs(t, table, ids...)
	}
}
