//go:build integration

package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// freeSeedIDs deletes, across every workspace, the rows whose explicit id a recorded seed is about to insert. The
// suites share one database, so another test may have left a row with one of those ids; the seed's ids are fixed by
// the recordings and cannot move. They must stay below testpg.SeqFloor: no sequence (and no testpg.ReserveIDs block)
// ever returns there, so a default-id insert anywhere never lands on a seeded row and the sequence is never moved.
func freeSeedIDs(t *testing.T, tbl string, cols []string, rows json.RawMessage) {
	t.Helper()
	if !slices.Contains(cols, "id") {
		return
	}
	var ids []struct {
		ID any `json:"id"`
	}
	if err := json.Unmarshal(rows, &ids); err != nil {
		t.Fatalf("%s seed ids: %v", tbl, err)
	}
	for _, r := range ids {
		if n, ok := r.ID.(float64); ok && n >= testpg.SeqFloor { // text ids have no sequence
			t.Fatalf("%s seed id %v is not below testpg.SeqFloor", tbl, n)
		}
	}
	ownerExec(t, `DELETE FROM `+tbl+` WHERE id IN (SELECT id FROM json_populate_recordset(null::`+tbl+`, $1::json))`, string(rows))
}
