//go:build integration

package outreachdraft

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

var uuidRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func TestCreateGroundedDraftMatchesPython(t *testing.T) {
	ctx := context.Background()
	pool := testpg.NewAppPool(t, 4)
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	ws := fmt.Sprintf("od_%d", os.Getpid())
	clean := func() { _, _ = owner.Exec(ctx, `DELETE FROM outreach_drafts WHERE workspace_id = $1`, ws) }
	clean()
	t.Cleanup(clean)
	_, create := loadVectors(t)
	for _, c := range create {
		var rec *wbcrud.Obj
		err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			var err error
			rec, err = Create(ctx, tx, Request{WorkspaceID: ws, ConversationID: "conv-1", SourceActionID: c.SourceActionID,
				RequestedPersonID: c.Requested, AllowRisky: c.AllowRisky, IdempotencyKey: c.Key,
				Source: tree(t, c.Source).(*wbcrud.Obj)})
			if _, isRefusal := err.(*Error); isRefusal {
				return err // roll back like Python's failed request
			}
			return err
		})
		if c.Error != "" {
			de, ok := err.(*Error)
			if !ok || de.Msg != c.Error {
				t.Fatalf("%s: err %v, want %q", c.Name, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		got := strings.ReplaceAll(uuidRE.ReplaceAllString(dumpT(t, rec), "$UUID"), ws, "ws-1")
		if want := uuidRE.ReplaceAllString(py(t, c.Receipt), "$UUID"); got != want {
			t.Fatalf("%s receipt:\n got %s\nwant %s", c.Name, got, want)
		}
		id, _ := rec.Get("draft_id")
		var conv, src, snap, state string
		var sent *string
		if err := owner.QueryRow(ctx, `SELECT conversation_id, source_action_id, source_snapshot::text, state, sent_at::text
			FROM outreach_drafts WHERE id = $1`, id).Scan(&conv, &src, &snap, &state, &sent); err != nil {
			t.Fatal(err)
		}
		if conv != c.Row.ConversationID || src != c.Row.SourceActionID || state != "draft" || sent != nil {
			t.Fatalf("%s row: %s %s %s %v", c.Name, conv, src, state, sent)
		}
		if want := py(t, c.Row.SourceSnapshot); snap != want {
			t.Fatalf("%s snapshot:\n got %s\nwant %s", c.Name, snap, want)
		}
	}
}
