//go:build integration

package leadpeople

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// M4: the Postgres ledger records each paid contact call of an action in the workspace
// scope, settles it once, and a replay of the same action never pays again.
func TestContactsLedgerSpend(t *testing.T) {
	app := testpg.NewAppPool(t, 4)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		_, _ = owner.Exec(context.Background(), `DELETE FROM workbook_spend_attempts WHERE workspace_id = 'wp-spend'`)
	}
	clean()
	t.Cleanup(func() { clean(); owner.Close() })

	disc := &fakeDisc{script: map[string]any{"hunter_io": found("jane@stripe.com")}} // prospeo: unavailable
	ver := &fakeVerify{script: map[string]any{"*": map[string]any{"status": "valid", "confidence": "verified", "source": "reacher"}}}
	s := &Service{Discover: disc, Verify: ver, Spend: LedgerSpend{Pool: app}, Concurrency: 1}
	run := func() map[string]any {
		got, err := s.EnrichPeopleContacts(context.Background(), "Stripe", "Sales", []map[string]any{{"name": "Jane Doe"}},
			ContactOptions{CompanyResolution: stripeRes, WorkspaceID: "wp-spend", ActionID: "act-1"})
		if err != nil || got["ok"] != true {
			t.Fatalf("%v %v", got, err)
		}
		return got
	}
	run()
	rows, err := owner.Query(context.Background(), `SELECT provider, status, coalesce(settled_microusd, -1) FROM workbook_spend_attempts
		WHERE workspace_id='wp-spend' AND workbook_id='~workspace' AND run_id='contacts:act-1' ORDER BY provider`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for rows.Next() {
		var p, st string
		var c int64
		if err := rows.Scan(&p, &st, &c); err != nil {
			t.Fatal(err)
		}
		if st != "settled" {
			t.Fatalf("%s %s", p, st)
		}
		got[p] = c
	}
	if len(got) != 3 || got["discovery:hunter_io"] != 40000 || got["discovery:prospeo"] != 0 || got["verification:verification_cascade"] != 0 {
		t.Fatalf("attempts %v", got)
	}
	// Replaying the same action reuses the attempts: no provider is called again.
	calls := len(disc.calls)
	out := run()
	if len(disc.calls) != calls {
		t.Fatalf("replay called providers again")
	}
	a := out["people"].([]map[string]any)[0]["contactability"].(map[string]any)["attempts"].([]map[string]any)[0]
	if a["detail"] != "spend_attempt_already_used" {
		t.Fatalf("attempt %v", a)
	}
}
