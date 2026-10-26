package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chatmemory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

const memorySnippetRunes = 200

// CaptureAndStore runs stream (a chat turn emitting through t.emit) while accumulating the assistant
// text and persisting every tool_result as a "tool" row, then stores the assistant row and a best-effort
// episodic memory. Like Python's _stream_with_storage nothing is stored after a client disconnect (the
// generator is cancelled there); every other outcome, including error frames, stores what was streamed.
// Persistence errors are logged without content and never break the stream. stream's error is returned.
func (t *Turn) CaptureAndStore(ctx context.Context, lastUserMsg string, stream func(ctx context.Context) error) error {
	var parts []string
	prev := t.OnEvent
	t.OnEvent = func(raw []byte) {
		if prev != nil {
			prev(raw)
		}
		t.captureEvent(ctx, raw, &parts)
	}
	defer func() { t.OnEvent = prev }()

	err := stream(ctx)
	if ctx.Err() != nil {
		return err
	}
	if text := strings.Join(parts, ""); text != "" {
		t.storeMessage(ctx, "assistant", text, nil)
		if lastUserMsg != "" {
			t.storeMemory(ctx, lastUserMsg, text)
		}
	}
	return err
}

// captureEvent inspects one emitted data frame (already Python-formatted JSON).
func (t *Turn) captureEvent(ctx context.Context, raw []byte, parts *[]string) {
	var ev map[string]json.RawMessage
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	if c, ok := ev["content"]; ok {
		var s string
		if json.Unmarshal(c, &s) == nil {
			*parts = append(*parts, s)
		}
	}
	tr, ok := ev["tool_result"]
	if !ok {
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(tr, &obj) != nil || obj == nil {
		return
	}
	name, hasName := obj["name"]
	label := "tool"
	if hasName {
		label = pyLabel(name)
	} else {
		name = json.RawMessage("null")
	}
	result, hasResult := obj["result"]
	if !hasResult {
		result = json.RawMessage("{}")
	}
	var b bytes.Buffer
	b.WriteString(`{"name": `)
	b.Write(name)
	b.WriteString(`, "result": `)
	b.Write(result)
	b.WriteByte('}')
	td, err := ssestream.Reformat(b.Bytes())
	if err != nil {
		t.logger().Warn("copilot tool row skipped: unrepresentable result", "conversation", t.ConvID)
		return
	}
	s := string(td)
	t.storeMessage(ctx, "tool", label+" result", &s)
}

// pyLabel renders a JSON value like an f-string of the decoded Python value (names are strings in practice).
func pyLabel(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	if string(bytes.TrimSpace(v)) == "null" {
		return "None"
	}
	return string(v)
}

// storeMemory is the best-effort episodic memory; slices are by code point like Python's [:200].
func (t *Turn) storeMemory(ctx context.Context, user, answer string) {
	defer func() {
		if r := recover(); r != nil {
			t.logger().Warn("copilot memory write panicked", "conversation", t.ConvID)
		}
	}()
	ns, err := chatmemory.Namespace(t.WS, t.UID)
	if err == nil {
		text := "User asked: " + runePrefix(user, memorySnippetRunes) + "\nAssistant answered about: " + runePrefix(answer, memorySnippetRunes)
		err = t.inTx(ctx, func(tx pgx.Tx) error {
			return chatmemory.Add(ctx, tx, ns, text, map[string]any{"conversation_id": t.ConvID, "type": "episodic"})
		})
	}
	if err != nil {
		t.logger().Warn("copilot memory write failed", "conversation", t.ConvID, "reason", fmt.Sprintf("%T", errors.Unwrap(err)))
	}
}

func runePrefix(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
