//go:build integration

package collect

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// L1: re-finding a stored company never overwrites what the user owns (status, notes, score) or
// what is already known; collection only fills empty columns.
func TestUpsertKeepsUserOwnedFields(t *testing.T) {
	pool, ctx := itSetup(t)
	s := PgStore{pool, itWS}
	a := NewLead(Lead{Company: "Acme", City: "Pune", Email: "sales@acme.com", Description: "old", Score: 50,
		CollectionJobID: "job-a"})
	if err := s.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	// The user works the lead.
	if err := tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE leads SET status = 'contacted', notes = 'call back Tuesday', score = 88,
			score_tier = 'hot', decision_makers = '[{"name": "Jane"}]' WHERE id = $1`, a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b := NewLead(Lead{Company: "Acme", City: "Pune", Email: "info@acme.com", Phone: "+91 20 1234 5678",
		Description: "new scraped text", Score: 30, DecisionMakers: `[{"name": "Mallory"}]`, CollectionJobID: "job-b"})
	if err := s.Upsert(ctx, b); err != nil || b.ID != a.ID {
		t.Fatalf("upsert: %v %d %d", err, a.ID, b.ID)
	}
	var status, notes, tier, email, phone, desc, dms, jobID string
	var score int32
	_ = tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, notes, score, score_tier, email, phone, description, decision_makers,
			collection_job_id FROM leads WHERE id = $1`, a.ID).Scan(&status, &notes, &score, &tier, &email, &phone, &desc, &dms, &jobID)
	})
	if status != "contacted" || notes != "call back Tuesday" || score != 88 || tier != "hot" {
		t.Errorf("user fields overwritten: %q %q %d %q", status, notes, score, tier)
	}
	if email != "sales@acme.com" || desc != "old" || dms != `[{"name": "Jane"}]` || jobID != "job-a" {
		t.Errorf("known fields overwritten: %q %q %q %q", email, desc, dms, jobID)
	}
	if phone != "+91 20 1234 5678" {
		t.Errorf("empty phone not filled: %q", phone)
	}
}
