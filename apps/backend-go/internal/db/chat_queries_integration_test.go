//go:build integration

package db_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

var tsShape = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)

func chatPool(t *testing.T, wss ...string) *pgxpool.Pool {
	t.Helper()
	pool := testpg.NewAppPool(t, 2)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		ctx := context.Background()
		_, _ = owner.Exec(ctx, `DELETE FROM chat_conversations WHERE workspace_id = ANY($1)`, wss)
		_, _ = owner.Exec(ctx, `DELETE FROM chat_tool_approvals WHERE workspace_id = ANY($1)`, wss)
		_, _ = owner.Exec(ctx, `DELETE FROM chat_memories WHERE user_id LIKE 't4mem_%'`)
		_, _ = owner.Exec(ctx, `DELETE FROM person_intel WHERE username LIKE 't4person_%'`)
	}
	clean()
	t.Cleanup(func() { clean(); owner.Close() })
	return pool
}

func inWS(t *testing.T, pool *pgxpool.Pool, ws string, fn func(q *gen.Queries) error) {
	t.Helper()
	err := tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error { return fn(gen.New(tx)) })
	if err != nil {
		t.Fatal(err)
	}
}

func TestChatConversationQueries(t *testing.T) {
	pid := os.Getpid()
	wsA, wsB := fmt.Sprintf("t4a_%d", pid), fmt.Sprintf("t4b_%d", pid)
	pool := chatPool(t, wsA, wsB)
	ctx := context.Background()

	inWS(t, pool, wsA, func(q *gen.Queries) error {
		for _, id := range []string{"c1", "c2"} {
			if err := q.ChatCreateConversation(ctx, gen.ChatCreateConversationParams{ID: id, Title: "New Chat", WorkspaceID: wsA, UserID: "u1"}); err != nil {
				return err
			}
		}
		return nil
	})
	// now() is the transaction start, so the bump must happen in a later one.
	time.Sleep(20 * time.Millisecond)
	inWS(t, pool, wsA, func(q *gen.Queries) error {
		// Add a message to c1: it bumps c1 above c2 in the list.
		if _, err := q.ChatAddMessage(ctx, gen.ChatAddMessageParams{ID: "m1", ConversationID: "c1", WorkspaceID: wsA, UserID: "u1", Role: "user", Content: "hi"}); err != nil {
			return err
		}
		// Foreign user / missing conversation: no row, nothing inserted.
		for _, c := range []gen.ChatAddMessageParams{
			{ID: "m2", ConversationID: "c1", WorkspaceID: wsA, UserID: "u2", Role: "user", Content: "x"},
			{ID: "m3", ConversationID: "nope", WorkspaceID: wsA, UserID: "u1", Role: "user", Content: "x"},
		} {
			if _, err := q.ChatAddMessage(ctx, c); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("add to %s/%s: err=%v, want ErrNoRows", c.ConversationID, c.UserID, err)
			}
		}
		list, err := q.ChatListConversations(ctx, gen.ChatListConversationsParams{WorkspaceID: wsA, UserID: "u1", RowLimit: 50})
		if err != nil {
			return err
		}
		if len(list) != 2 || list[0].ID != "c1" || !tsShape.MatchString(list[0].CreatedAt) || !tsShape.MatchString(list[0].UpdatedAt) {
			t.Errorf("list = %+v", list)
		}
		if l1, _ := q.ChatListConversations(ctx, gen.ChatListConversationsParams{WorkspaceID: wsA, UserID: "u1", RowLimit: 1}); len(l1) != 1 {
			t.Errorf("limit 1 returned %d", len(l1))
		}
		msgs, err := q.ChatGetMessages(ctx, gen.ChatGetMessagesParams{ConversationID: "c1", WorkspaceID: wsA, UserID: "u1"})
		if err != nil || len(msgs) != 1 || msgs[0].Content != "hi" || msgs[0].ToolData.Valid || !tsShape.MatchString(msgs[0].CreatedAt) {
			t.Errorf("messages = %+v err=%v", msgs, err)
		}
		if other, _ := q.ChatGetMessages(ctx, gen.ChatGetMessagesParams{ConversationID: "c1", WorkspaceID: wsA, UserID: "u2"}); len(other) != 0 {
			t.Errorf("foreign user read %d messages", len(other))
		}
		if err := q.ChatUpdateTitle(ctx, gen.ChatUpdateTitleParams{ID: "c1", Title: "T", WorkspaceID: wsA, UserID: "u2"}); err != nil {
			return err
		}
		if c, err := q.ChatGetConversation(ctx, gen.ChatGetConversationParams{ID: "c1", WorkspaceID: wsA, UserID: "u1"}); err != nil || c.Title != "New Chat" {
			t.Errorf("foreign title update leaked: %+v %v", c, err)
		}
		if err := q.ChatUpdateTitle(ctx, gen.ChatUpdateTitleParams{ID: "c1", Title: "T", WorkspaceID: wsA, UserID: "u1"}); err != nil {
			return err
		}
		if c, _ := q.ChatGetConversation(ctx, gen.ChatGetConversationParams{ID: "c1", WorkspaceID: wsA, UserID: "u1"}); c.Title != "T" {
			t.Errorf("title = %q", c.Title)
		}
		return nil
	})

	// Cross-tenant: same ids invisible and undeletable from workspace B.
	inWS(t, pool, wsB, func(q *gen.Queries) error {
		if _, err := q.ChatGetConversation(ctx, gen.ChatGetConversationParams{ID: "c1", WorkspaceID: wsA, UserID: "u1"}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("cross-tenant get err=%v, want ErrNoRows (RLS)", err)
		}
		return q.ChatDeleteConversation(ctx, gen.ChatDeleteConversationParams{ID: "c1", WorkspaceID: wsA, UserID: "u1"})
	})

	inWS(t, pool, wsA, func(q *gen.Queries) error {
		if _, err := q.ChatGetConversation(ctx, gen.ChatGetConversationParams{ID: "c1", WorkspaceID: wsA, UserID: "u1"}); err != nil {
			t.Errorf("c1 vanished after foreign delete: %v", err)
		}
		if err := q.ChatDeleteConversation(ctx, gen.ChatDeleteConversationParams{ID: "c1", WorkspaceID: wsA, UserID: "u1"}); err != nil {
			return err
		}
		if m, _ := q.ChatGetMessages(ctx, gen.ChatGetMessagesParams{ConversationID: "c1", WorkspaceID: wsA, UserID: "u1"}); len(m) != 0 {
			t.Errorf("messages survived cascade: %d", len(m))
		}
		return nil
	})
}

func TestChatConsumeApprovalAtomic(t *testing.T) {
	ws := fmt.Sprintf("t4ap_%d", os.Getpid())
	pool := chatPool(t, ws)
	ctx := context.Background()
	now := time.Now().Unix()
	inWS(t, pool, ws, func(q *gen.Queries) error {
		for id, at := range map[string]int64{"fresh": now, "old": now - 7200} {
			if err := q.ChatCreateApproval(ctx, gen.ChatCreateApprovalParams{ID: id, WorkspaceID: ws, UserID: "u1", ToolCallJson: `{"name":"x"}`, CreatedAtEpoch: at}); err != nil {
				return err
			}
		}
		consume := func(id, user, decision string) (string, error) {
			return q.ChatConsumeApproval(ctx, gen.ChatConsumeApprovalParams{ID: id, WorkspaceID: ws, UserID: user, Decision: decision, MinCreatedAtEpoch: now - 3600})
		}
		if _, err := consume("fresh", "u2", "approve"); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("foreign user consume err=%v", err)
		}
		if _, err := consume("old", "u1", "approve"); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("expired consume err=%v", err)
		}
		if tc, err := consume("fresh", "u1", "approve"); err != nil || tc != `{"name":"x"}` {
			t.Errorf("first consume = %q, %v", tc, err)
		}
		if _, err := consume("fresh", "u1", "approve"); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("second consume err=%v, want ErrNoRows (single use)", err)
		}
		return nil
	})
}

func TestMemoryAndPersonQueries(t *testing.T) {
	pool := chatPool(t)
	ctx := context.Background()
	u := fmt.Sprintf("t4mem_%d", os.Getpid())
	q := gen.New(pool)
	for i := 0; i < 3; i++ {
		if err := q.MemAdd(ctx, gen.MemAddParams{UserID: u, Text: fmt.Sprintf("m%d", i), Metadata: []byte(`{"k":1}`), CreatedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := q.MemAll(ctx, u)
	if err != nil || len(all) != 3 || all[0].Text != "m2" {
		t.Fatalf("MemAll = %+v %v", all, err)
	}
	rec, err := q.MemRecent(ctx, u)
	if err != nil || len(rec) != 3 || rec[2].Text != "m0" {
		t.Fatalf("MemRecent = %+v %v", rec, err)
	}

	var ids []int32
	for i := 0; i < 3; i++ {
		id, err := q.PersonInsert(ctx, gen.PersonInsertParams{
			LinkedinUrl: pgtype.Text{String: "https://linkedin.com/in/x", Valid: true},
			Username:    pgtype.Text{String: fmt.Sprintf("t4person_%d", i), Valid: true},
			Emails:      []byte(`["a@b.co"]`), SocialLinks: []byte(`{}`), Articles: []byte(`[]`), Mentions: []byte(`[]`),
			Companies: []byte(`[]`), Education: []byte(`[]`), Skills: []byte(`[]`), RawSources: []byte(`{}`),
			Status:    pgtype.Text{String: "done", Valid: true},
			CreatedAt: pgtype.Timestamp{Time: time.Now().UTC(), Valid: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	hist, err := q.PersonHistory(ctx, gen.PersonHistoryParams{RowSkip: 1, RowLimit: 1})
	if err != nil || len(hist) != 1 || hist[0].ID != ids[1] {
		t.Fatalf("PersonHistory(skip 1, limit 1) = %+v %v (ids %v)", hist, err, ids)
	}
	got, err := q.PersonGet(ctx, ids[0])
	if err != nil || got.Username.String != "t4person_0" {
		t.Fatalf("PersonGet = %+v %v", got, err)
	}
	for i, want := range []int64{1, 0} {
		if n, err := q.PersonDelete(ctx, ids[0]); err != nil || n != want {
			t.Fatalf("PersonDelete #%d = %d %v", i, n, err)
		}
	}
	if _, err := q.PersonGet(ctx, ids[0]); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("PersonGet after delete err=%v", err)
	}
}
