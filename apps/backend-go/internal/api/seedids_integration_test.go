//go:build integration

package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// freeSeedIDs deletes, across every workspace, the rows whose explicit id a recorded seed is about to insert. The
// suites share one database, so another test (or package) may have left a sequence-generated row on one of those
// ids; the seed's ids are fixed by the recordings and cannot move.
func freeSeedIDs(t *testing.T, tbl string, cols []string, rows json.RawMessage) {
	t.Helper()
	if !slices.Contains(cols, "id") {
		return
	}
	ownerExec(t, `DELETE FROM `+tbl+` WHERE id IN (SELECT id FROM json_populate_recordset(null::`+tbl+`, $1::json))`, string(rows))
}

// advanceSeedSeq moves a serial id sequence past the explicit ids a seed inserted, so later inserts that take the
// default id do not collide with the seeded rows. Tables without a serial id are left alone.
func advanceSeedSeq(t *testing.T, tbl string) {
	t.Helper()
	ctx := context.Background()
	c := audOwner(t)
	var seq *string
	if err := c.QueryRow(ctx, `SELECT pg_get_serial_sequence($1, 'id')`, tbl).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq == nil {
		return
	}
	if _, err := c.Exec(ctx, `SELECT setval($1, GREATEST((SELECT max(id) FROM `+tbl+`), 1))`, *seq); err != nil {
		t.Fatalf("setval %s: %v", tbl, err)
	}
}
