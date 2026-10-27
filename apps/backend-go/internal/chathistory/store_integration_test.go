//go:build integration

package chathistory_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func ws() string { return "ws-t8-" + uuid.NewString()[:8] }

func in(t *testing.T, pool *pgxpool.Pool, w string, fn func(pgx.Tx)) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), pool, w, func(tx pgx.Tx) error { fn(tx); return nil }); err != nil {
		t.Fatal(err)
	}
}

func uid(n int32) *int32 { return &n }

func mustCreate(t *testing.T, pool *pgxpool.Pool, w string, u *int32, title string) chathistory.Conversation {
	t.Helper()
	var c chathistory.Conversation
	in(t, pool, w, func(tx pgx.Tx) {
		var err error
		c, err = chathistory.Create(context.Background(), tx, w, u, title)
		if err != nil {
			t.Fatal(err)
		}
	})
	return c
}

func add(t *testing.T, pool *pgxpool.Pool, w, conv, role, content string, td *string) error {
	t.Helper()
	var err error
	// own transaction per message so now() differs (tx-shared now() has undefined order)
	_ = tenancy.InWorkspace(context.Background(), pool, w, func(tx pgx.Tx) error {
		err = chathistory.AddMessage(context.Background(), tx, conv, role, content, td, w, uid(1))
		return nil
	})
	return err
}

func TestOwnerOnlyReads(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	w := ws()
	c := mustCreate(t, pool, w, uid(1), "mine")
	in(t, pool, w, func(tx pgx.Tx) {
		ctx := context.Background()
		if g, err := chathistory.Get(ctx, tx, c.ID, w, uid(1)); err != nil || g == nil || g.Title != "mine" {
			t.Fatalf("owner get: %v %v", g, err)
		}
		if g, err := chathistory.Get(ctx, tx, c.ID, w, uid(2)); err != nil || g != nil {
			t.Fatalf("other user get: %v %v", g, err)
		}
		if g, _ := chathistory.Get(ctx, tx, c.ID, w, nil); g != nil {
			t.Fatal("self-host namespace must not see user 1's conversation")
		}
		if l, _ := chathistory.List(ctx, tx, w, uid(2), 50); len(l) != 0 {
			t.Fatal("other user list")
		}
		if m, _ := chathistory.Messages(ctx, tx, c.ID, w, uid(2)); len(m) != 0 {
			t.Fatal("other user messages")
		}
	})
}

func TestSelfHostNilUser(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	c := mustCreate(t, pool, w, nil, "sh")
	in(t, pool, w, func(tx pgx.Tx) {
		var u string
		if err := tx.QueryRow(context.Background(), `SELECT user_id FROM chat_conversations WHERE id=$1`, c.ID).Scan(&u); err != nil || u != "self-host" {
			t.Fatalf("user_id %q %v", u, err)
		}
		if g, _ := chathistory.Get(context.Background(), tx, c.ID, w, nil); g == nil {
			t.Fatal("nil uid reads own")
		}
	})
}

func TestCrossWorkspaceInvisible(t *testing.T) {
	pool := testpg.NewAppPool(t, 1) // one conn alternating tenants
	a, b := ws(), ws()
	c := mustCreate(t, pool, a, uid(1), "alpha")
	_ = add(t, pool, a, c.ID, "user", "hi", nil)
	in(t, pool, b, func(tx pgx.Tx) {
		ctx := context.Background()
		if g, _ := chathistory.Get(ctx, tx, c.ID, b, uid(1)); g != nil {
			t.Fatal("get leaked")
		}
		if l, _ := chathistory.List(ctx, tx, b, uid(1), 50); len(l) != 0 {
			t.Fatal("list leaked")
		}
		if m, _ := chathistory.Messages(ctx, tx, c.ID, b, uid(1)); len(m) != 0 {
			t.Fatal("messages leaked")
		}
	})
	if err := add(t, pool, b, c.ID, "user", "x", nil); !errors.Is(err, chathistory.ErrNoConversation) {
		t.Fatalf("add into foreign ws: %v", err)
	}
	in(t, pool, a, func(tx pgx.Tx) {
		if m, _ := chathistory.Messages(context.Background(), tx, c.ID, a, uid(1)); len(m) != 1 {
			t.Fatalf("alpha messages %d", len(m))
		}
	})
}

func TestDeleteForeignIsNoop(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w, other := ws(), ws()
	c := mustCreate(t, pool, w, uid(1), "keep")
	in(t, pool, w, func(tx pgx.Tx) {
		if err := chathistory.Delete(context.Background(), tx, c.ID, w, uid(2)); err != nil {
			t.Fatal(err)
		}
	})
	in(t, pool, other, func(tx pgx.Tx) {
		if err := chathistory.Delete(context.Background(), tx, c.ID, other, uid(1)); err != nil {
			t.Fatal(err)
		}
	})
	in(t, pool, w, func(tx pgx.Tx) {
		if g, _ := chathistory.Get(context.Background(), tx, c.ID, w, uid(1)); g == nil {
			t.Fatal("foreign delete removed row")
		}
		if err := chathistory.Delete(context.Background(), tx, c.ID, w, uid(1)); err != nil {
			t.Fatal(err)
		}
		if g, _ := chathistory.Get(context.Background(), tx, c.ID, w, uid(1)); g != nil {
			t.Fatal("owner delete failed")
		}
	})
}

func TestMessageRoleCheck(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	c := mustCreate(t, pool, w, uid(1), "r")
	for _, r := range []string{"user", "assistant", "system", "tool"} {
		if err := add(t, pool, w, c.ID, r, "ok", nil); err != nil {
			t.Fatalf("role %s: %v", r, err)
		}
	}
	for _, r := range []string{"admin", "", "USER"} {
		if err := add(t, pool, w, c.ID, r, "x", nil); !errors.Is(err, chathistory.ErrInvalidRole) {
			t.Fatalf("role %q: %v", r, err)
		}
	}
}

func TestMessageTooLarge(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	c := mustCreate(t, pool, w, uid(1), "big")
	if err := add(t, pool, w, c.ID, "user", strings.Repeat("a", chathistory.MaxContentBytes), nil); err != nil {
		t.Fatalf("exactly 1 MiB: %v", err)
	}
	if err := add(t, pool, w, c.ID, "user", strings.Repeat("a", chathistory.MaxContentBytes+1), nil); !errors.Is(err, chathistory.ErrContentTooLarge) {
		t.Fatalf("over: %v", err)
	}
	td := strings.Repeat("b", chathistory.MaxContentBytes+1)
	if err := add(t, pool, w, c.ID, "tool", "x", &td); !errors.Is(err, chathistory.ErrContentTooLarge) {
		t.Fatalf("tool_data over: %v", err)
	}
}

func TestMessagesOrderByCreatedAtThenRowOrder(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	c := mustCreate(t, pool, w, uid(1), "ord")
	td := `{"a":1}`
	// user and assistant each in their own transaction (distinct now())
	want := []string{"first", "second", "third"}
	for i, s := range want {
		role := "user"
		var p *string
		if i == 1 {
			role = "assistant"
		}
		if i == 2 {
			role, p = "tool", &td
		}
		if err := add(t, pool, w, c.ID, role, s, p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	in(t, pool, w, func(tx pgx.Tx) {
		ms, err := chathistory.Messages(context.Background(), tx, c.ID, w, uid(1))
		if err != nil || len(ms) != 3 {
			t.Fatalf("%v %d", err, len(ms))
		}
		for i, m := range ms {
			if m.Content != want[i] || m.ConversationID != c.ID || len(m.ID) != 12 {
				t.Fatalf("pos %d: %+v", i, m)
			}
		}
		if ms[0].ToolData != nil || ms[2].ToolData == nil || *ms[2].ToolData != td {
			t.Fatalf("tool_data: %+v", ms)
		}
	})
	// AddMessage bumps updated_at
	in(t, pool, w, func(tx pgx.Tx) {
		var bumped bool
		if err := tx.QueryRow(context.Background(), `SELECT updated_at > created_at FROM chat_conversations WHERE id=$1`, c.ID).Scan(&bumped); err != nil || !bumped {
			t.Fatalf("updated_at not bumped: %v %v", bumped, err)
		}
	})
}

func TestListLimitUpdateTitle(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	c1 := mustCreate(t, pool, w, uid(1), "one")
	time.Sleep(5 * time.Millisecond)
	c2 := mustCreate(t, pool, w, uid(1), "two")
	time.Sleep(5 * time.Millisecond)
	_ = add(t, pool, w, c1.ID, "user", "bump", nil)
	in(t, pool, w, func(tx pgx.Tx) {
		ctx := context.Background()
		l, err := chathistory.List(ctx, tx, w, uid(1), 50)
		if err != nil || len(l) != 2 || l[0].ID != c1.ID || l[1].ID != c2.ID {
			t.Fatalf("list by updated_at desc: %+v %v", l, err)
		}
		if l, _ := chathistory.List(ctx, tx, w, uid(1), 1); len(l) != 1 {
			t.Fatal("limit")
		}
		if err := chathistory.UpdateTitle(ctx, tx, c2.ID, "renamed", w, uid(2)); err != nil {
			t.Fatal(err)
		}
		if g, _ := chathistory.Get(ctx, tx, c2.ID, w, uid(1)); g.Title != "two" {
			t.Fatal("foreign title update applied")
		}
		if err := chathistory.UpdateTitle(ctx, tx, c2.ID, "renamed", w, uid(1)); err != nil {
			t.Fatal(err)
		}
		if g, _ := chathistory.Get(ctx, tx, c2.ID, w, uid(1)); g.Title != "renamed" {
			t.Fatal("title update")
		}
	})
}

// ---- approvals ----

func mkApproval(t *testing.T, pool *pgxpool.Pool, w string, u *int32, call map[string]any) string {
	t.Helper()
	var id string
	in(t, pool, w, func(tx pgx.Tx) {
		var err error
		id, err = chathistory.CreateApproval(context.Background(), tx, w, u, call)
		if err != nil {
			t.Fatal(err)
		}
	})
	return id
}

func consume(t *testing.T, pool *pgxpool.Pool, id, w string, u *int32, decision string, now time.Time) (map[string]any, bool) {
	t.Helper()
	var call map[string]any
	var ok bool
	in(t, pool, w, func(tx pgx.Tx) {
		var err error
		call, ok, err = chathistory.ConsumeApproval(context.Background(), tx, id, w, u, decision, time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
	})
	return call, ok
}

var sample = map[string]any{"id": "call_1", "name": "update_lead_status", "args": map[string]any{"z": "é", "a": []any{"x", true, nil}}}

func TestApprovalConsumeOnce(t *testing.T) {
	pool := testpg.NewAppPool(t, 16)
	w := ws()
	id := mkApproval(t, pool, w, uid(1), sample)
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tenancy.InWorkspace(context.Background(), pool, w, func(tx pgx.Tx) error {
				_, ok, err := chathistory.ConsumeApproval(context.Background(), tx, id, w, uid(1), "approve", time.Hour, time.Now())
				if err != nil {
					t.Error(err)
				}
				if ok {
					atomic.AddInt32(&wins, 1)
				}
				return nil
			})
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
}

func TestApprovalExpired(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	var epoch int64
	mk := func() string {
		id := mkApproval(t, pool, w, uid(1), sample)
		in(t, pool, w, func(tx pgx.Tx) {
			if err := tx.QueryRow(context.Background(), `SELECT created_at_epoch FROM chat_tool_approvals WHERE id=$1`, id).Scan(&epoch); err != nil {
				t.Fatal(err)
			}
		})
		return id
	}
	id := mk()
	if _, ok := consume(t, pool, id, w, uid(1), "approve", time.Unix(epoch+3601, 0)); ok {
		t.Fatal("3601s old must be rejected")
	}
	// rejection must not burn the approval: still pending and inside the window
	if _, ok := consume(t, pool, id, w, uid(1), "approve", time.Unix(epoch+3599, 0)); !ok {
		t.Fatal("3599s old must be accepted")
	}
	id = mk()
	if _, ok := consume(t, pool, id, w, uid(1), "approve", time.Unix(epoch+3600, 0)); !ok {
		t.Fatal("exactly ttl accepted (Python rejects only > ttl)")
	}
}

func TestApprovalWrongTenant(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w, other := ws(), ws()
	id := mkApproval(t, pool, w, uid(1), sample)
	now := time.Now()
	if _, ok := consume(t, pool, id, other, uid(1), "approve", now); ok {
		t.Fatal("wrong workspace")
	}
	if _, ok := consume(t, pool, id, w, uid(2), "approve", now); ok {
		t.Fatal("wrong user")
	}
	if _, ok := consume(t, pool, id, w, nil, "approve", now); ok {
		t.Fatal("self-host namespace must not consume user 1's approval")
	}
	if _, ok := consume(t, pool, id, w, uid(1), "approve", now); !ok {
		t.Fatal("owner still can (foreign attempts did not burn it)")
	}
}

func TestApprovalDenyThenApproveFails(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	id := mkApproval(t, pool, w, uid(1), sample)
	if _, ok := consume(t, pool, id, w, uid(1), "deny", time.Now()); !ok {
		t.Fatal("deny")
	}
	if _, ok := consume(t, pool, id, w, uid(1), "approve", time.Now()); ok {
		t.Fatal("approve after deny")
	}
}

func TestApprovalInvalidInputs(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	id := mkApproval(t, pool, w, uid(1), sample)
	now := time.Now()
	for _, d := range []string{"", "maybe", "APPROVE"} {
		if _, ok := consume(t, pool, id, w, uid(1), d, now); ok {
			t.Fatalf("decision %q", d)
		}
	}
	var call map[string]any
	var ok bool
	var err error
	in(t, pool, w, func(tx pgx.Tx) {
		call, ok, err = chathistory.ConsumeApproval(context.Background(), tx, "", w, uid(1), "approve", time.Hour, now)
	})
	if err != nil || ok || call != nil {
		t.Fatal("empty id")
	}
	if _, ok := consume(t, pool, id, w, uid(1), "approve", now); !ok {
		t.Fatal("invalid attempts must not burn the approval")
	}
}

func TestApprovalStoredCanonical(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	id := mkApproval(t, pool, w, uid(1), sample)
	in(t, pool, w, func(tx pgx.Tx) {
		var s, u string
		if err := tx.QueryRow(context.Background(), `SELECT tool_call_json, user_id FROM chat_tool_approvals WHERE id=$1`, id).Scan(&s, &u); err != nil {
			t.Fatal(err)
		}
		want := `{"args":{"a":["x",true,null],"z":"\u00e9"},"id":"call_1","name":"update_lead_status"}`
		if s != want {
			t.Fatalf("stored %q want %q", s, want)
		}
		if u != "1" {
			t.Fatalf("user %s", u)
		}
	})
	if len(id) != 32 {
		t.Fatalf("approval id should be uuid hex, got %q", id)
	}
}

func TestApprovalReturnedIDIsApprovalID(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	id := mkApproval(t, pool, w, nil, sample)
	call, ok := consume(t, pool, id, w, nil, "approve", time.Now())
	if !ok || call["id"] != id || call["name"] != "update_lead_status" {
		t.Fatalf("%v %v", call, ok)
	}
	if args, _ := call["args"].(map[string]any); args["z"] != "é" {
		t.Fatalf("args %v", call["args"])
	}
}

// Messages written in one transaction share created_at (now()); the read order must still be defined: by id.
func TestMessagesSharingCreatedAtComeBackInIDOrder(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	w := ws()
	c := mustCreate(t, pool, w, uid(1), "tie")
	in(t, pool, w, func(tx pgx.Tx) {
		for i := 0; i < 12; i++ {
			if err := chathistory.AddMessage(context.Background(), tx, c.ID, "user", "m", nil, w, uid(1)); err != nil {
				t.Fatal(err)
			}
		}
		ms, err := chathistory.Messages(context.Background(), tx, c.ID, w, uid(1))
		if err != nil || len(ms) != 12 {
			t.Fatalf("%v %d", err, len(ms))
		}
		for i := 1; i < len(ms); i++ {
			if ms[i-1].CreatedAt != ms[i].CreatedAt || ms[i-1].ID >= ms[i].ID {
				t.Fatalf("order at %d: %+v %+v", i, ms[i-1], ms[i])
			}
		}
	})
}
