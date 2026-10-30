//go:build integration

package records

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
)

func TestDeletedRecordDoesNotRetainImportIdentity(t *testing.T) {
	for _, kind := range []Kind{Company, Person} {
		t.Run(string(kind), func(t *testing.T) {
			s, ws := recordEnv(t)
			write := Write{Company: map[string]any{"name": "Temporary company"}}
			table := "company_entities"
			if kind == Person {
				write = Write{Person: map[string]any{"full_name": "Temporary person", "email": "temporary@example.test"}}
				table = "person_entities"
			}
			first := mustRecord(t, s, ws, kind, write, "import:deleted-record", "same-source-row")
			err := tenancy.InWorkspace(context.Background(), s.Pool, ws, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), "DELETE FROM "+table+" WHERE workspace_id=$1 AND id=$2", ws, first.ID)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			next := mustRecord(t, s, ws, kind, write, "import:deleted-record", "same-source-row")
			if next.ID == first.ID {
				t.Fatal("deleted identity was reused")
			}
		})
	}
}
