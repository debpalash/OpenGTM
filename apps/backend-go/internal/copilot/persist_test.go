//go:build integration

package copilot

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chatmemory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilotauth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

type persistEnv struct {
	*nativeEnv
	conv string
}

func persistSetup(t *testing.T) *persistEnv {
	t.Helper()
	e := &persistEnv{nativeEnv: nativeSetup(t)}
	err := tenancy.InWorkspace(context.Background(), e.pool, e.ws, func(tx pgx.Tx) error {
		c, err := chathistory.Create(context.Background(), tx, e.ws, i32(7), "t")
		e.conv = c.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM chat_messages WHERE workspace_id = $1`, e.ws)
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM chat_conversations WHERE workspace_id = $1`, e.ws)
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM chat_memories WHERE user_id LIKE $1`, e.ws+":%")
	})
	return e
}

func (e *persistEnv) turn() *Turn {
	d := Deps{Pool: e.pool, Settings: settingsStub{}}
	return &Turn{Deps: d, WS: e.ws, Slug: "alpha", UID: i32(7), ConvID: e.conv,
		Auth: copilotauth.Resolved{WorkspaceID: e.ws, Slug: "alpha", UserID: i32(7)}}
}

func (e *persistEnv) messages(t *testing.T) []chathistory.Message {
	t.Helper()
	var out []chathistory.Message
	err := tenancy.InWorkspace(context.Background(), e.pool, e.ws, func(tx pgx.Tx) error {
		var err error
		out, err = chathistory.Messages(context.Background(), tx, e.conv, e.ws, i32(7))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *persistEnv) memories(t *testing.T) []chatmemory.Memory {
	t.Helper()
	ns, _ := chatmemory.Namespace(e.ws, i32(7))
	var out []chatmemory.Memory
	err := tenancy.InWorkspace(context.Background(), e.pool, e.ws, func(tx pgx.Tx) error {
		var err error
		out, err = chatmemory.All(context.Background(), tx, ns)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// capture runs events through CaptureAndStore the way a chat turn would emit them.
func (e *persistEnv) capture(t *testing.T, tn *Turn, user string, events ...string) string {
	t.Helper()
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	err := tn.CaptureAndStore(context.Background(), user, func(ctx context.Context) error {
		for _, ev := range events {
			if err := tn.emit(w, []byte(ev)); err != nil {
				return err
			}
		}
		return w.Done()
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec.String()
}

func TestPersistToolRowShape(t *testing.T) {
	e := persistSetup(t)
	e.capture(t, e.turn(), "find people",
		`{"tool_result":{"name":"find_people_at_company","result":{"z":1,"a":[1.5,"—"],"n":null}}}`,
		`{"tool_result":{"name":"bare"}}`,
		`{"content":"done"}`)
	ms := e.messages(t)
	if len(ms) != 3 {
		t.Fatalf("rows=%d: %+v", len(ms), ms)
	}
	if ms[0].Role != "tool" || ms[0].Content != "find_people_at_company result" || ms[0].ToolData == nil ||
		*ms[0].ToolData != `{"name": "find_people_at_company", "result": {"z": 1, "a": [1.5, "\u2014"], "n": null}}` {
		t.Fatalf("tool row: %+v td=%q", ms[0], *ms[0].ToolData)
	}
	if ms[1].Content != "bare result" || *ms[1].ToolData != `{"name": "bare", "result": {}}` {
		t.Fatalf("bare row: %+v", ms[1])
	}
	if ms[2].Role != "assistant" || ms[2].Content != "done" || ms[2].ToolData != nil {
		t.Fatalf("assistant row: %+v", ms[2])
	}
}

func TestMemoryStoredBestEffort(t *testing.T) {
	e := persistSetup(t)
	user := strings.Repeat("é", 250)
	e.capture(t, e.turn(), user, `{"conversation_id":"x"}`, `{"content":"Hel"}`, `{"content":"lo "}`, `{"content":"`+strings.Repeat("w", 300)+`"}`)
	ms := e.memories(t)
	if len(ms) != 1 {
		t.Fatalf("memories=%d", len(ms))
	}
	want := "User asked: " + strings.Repeat("é", 200) + "\nAssistant answered about: Hello " + strings.Repeat("w", 194)
	if ms[0].Memory != want {
		t.Fatalf("memory text %q", ms[0].Memory)
	}
	if ms[0].Metadata["conversation_id"] != e.conv || ms[0].Metadata["type"] != "episodic" {
		t.Fatalf("metadata %+v", ms[0].Metadata)
	}
}

func TestMemoryFailureDoesNotBreakStream(t *testing.T) {
	e := persistSetup(t)
	tn := e.turn()
	tn.WS = "" // an empty workspace makes the memory namespace invalid
	out := e.capture(t, tn, "hi", `{"content":"answer"}`)
	if out != "data: {\"content\": \"answer\"}\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream %q", out)
	}
	if n := len(e.memories(t)); n != 0 {
		t.Fatalf("memories=%d", n)
	}
}

func TestNoAssistantRowWhenEmpty(t *testing.T) {
	e := persistSetup(t)
	e.capture(t, e.turn(), "hi", `{"conversation_id":"x"}`, `{"error":"boom"}`)
	if ms := e.messages(t); len(ms) != 0 {
		t.Fatalf("rows=%+v", ms)
	}
	if n := len(e.memories(t)); n != 0 {
		t.Fatalf("memories=%d", n)
	}
}

func TestNoMemoryWithoutUserMessage(t *testing.T) {
	e := persistSetup(t)
	e.capture(t, e.turn(), "", `{"content":"a"}`)
	if len(e.messages(t)) != 1 || len(e.memories(t)) != 0 {
		t.Fatal("assistant row only expected")
	}
}

func TestPersistenceErrorNeverBreaksStream(t *testing.T) {
	e := persistSetup(t)
	tn := e.turn()
	tn.ConvID = "does-not-exist" // every insert fails with ErrNoConversation
	out := e.capture(t, tn, "hi", `{"tool_result":{"name":"t","result":{}}}`, `{"content":"a"}`)
	if out != "data: {\"tool_result\": {\"name\": \"t\", \"result\": {}}}\n\ndata: {\"content\": \"a\"}\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream %q", out)
	}
}

func TestNoAssistantRowAfterClientDisconnect(t *testing.T) {
	e := persistSetup(t)
	tn := e.turn()
	ctx, cancel := context.WithCancel(context.Background())
	err := tn.CaptureAndStore(ctx, "hi", func(ctx context.Context) error {
		_ = tn.emit(func() *ssestream.Writer { w, _ := ssestream.New(newRec(), nil); return w }(), []byte(`{"content":"partial"}`))
		cancel()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("want the stream's error back")
	}
	if ms := e.messages(t); len(ms) != 0 {
		t.Fatalf("Python stores nothing after a disconnect: %+v", ms)
	}
}
