package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

// This file replaces POST /internal/copilot/llm (routers/internal_copilot.py _llm_stream) under
// GO_COPILOT_NATIVE: one provider round through the Go LLM client, translated into the same frames the
// worker sent, so Turn.round handles both sources identically.

// Stream caps of one round (internal_copilot MAX_STREAM_*): beyond them the round ends with an error frame.
const (
	minSecretLen    = 6
	anthropicTokens = 16000 // adaptive thinking shares max_tokens with the answer; 4096 (the OpenAI path) is too tight
)

// The caps are variables so tests can lower them. llm.Stream enforces the same limits on the provider's raw
// lines, bytes and tool-call argument deltas; these are the backstop on the frames the round delivers, and the
// wall clock also covers a provider call that never produces a frame (the Anthropic turn, a stalled dial).
var (
	maxRoundFrames  = 20000
	maxRoundBytes   = 8 << 20
	maxRoundSeconds = 300 * time.Second
)

// frameSource is one round's frames: *pyworker.Stream or the native round.
type frameSource interface {
	Next() (pyworker.Frame, error)
	Close()
}

// openRound starts one llm round on the native engine when configured, else on the Python worker.
func (t *Turn) openRound(ctx context.Context, rq roundRequest) (frameSource, error) {
	if t.Engine != nil && t.Engine.LLM != nil {
		return t.nativeRound(ctx, rq), nil
	}
	return t.PY.Stream(ctx, "/internal/copilot/llm", map[string]any{
		"workspace_id": t.WS, "messages": rq.messages, "tools_enabled": rq.toolsEnabled,
		"provider_cursor": rq.cursor, "round_budget_hit": rq.budgetHit,
	})
}

// chatChain is _provider_chain(): the configured chain, or the keyless default provider.
func (e *Engine) chatChain(ctx context.Context) []llm.ChatProvider {
	if e.Chain != nil {
		return e.Chain(ctx)
	}
	if c := e.LLM.ChatChain(ctx); len(c) > 0 {
		return c
	}
	return []llm.ChatProvider{e.LLM.ActiveChatProvider(ctx)}
}

// sanitizeMessages is _sanitize_messages: the OpenAI-spec cleaning applied before every round against the
// FIRST provider of the chain (Gemini rejects empty content).
func sanitizeMessages(msgs []map[string]any, head llm.ChatProvider) []map[string]any {
	gemini := strings.Contains(head.ID, "google") || strings.Contains(strings.ToLower(head.Model), "gemini")
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		var role any = "user"
		if r, ok := m["role"]; ok {
			role = r
		}
		c := map[string]any{"role": role}
		content := ""
		if v, ok := m["content"]; ok && v != nil {
			content = pyStr(v)
		}
		if content == "" && !pyTruthy(m["tool_calls"]) && gemini {
			content = " "
		}
		c["content"] = content
		if pyTruthy(m["tool_calls"]) {
			c["tool_calls"] = m["tool_calls"]
		}
		if v, ok := m["tool_call_id"]; ok {
			c["tool_call_id"] = v
		}
		if v, ok := m["name"]; ok {
			c["name"] = v
		}
		if role == "function" {
			c["role"] = "tool"
			if v, ok := m["name"]; ok {
				if _, has := c["tool_call_id"]; !has {
					c["tool_call_id"] = v
				}
			}
		}
		if c["role"] == "tool" {
			if _, has := c["tool_call_id"]; !has {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// scrub replaces every chain key (>= 6 chars) in s, like _scrub.
func scrub(s string, secrets []string) string {
	for _, k := range secrets {
		s = strings.ReplaceAll(s, k, "[redacted]")
	}
	return s
}

func indexOf(chain []llm.ChatProvider, id string) int {
	for i, p := range chain {
		if p.ID == id {
			return i
		}
	}
	return 0
}

// nativeSrc translates llm frames into worker frames with _llm_stream's held-terminal rules.
type nativeSrc struct {
	ctx     context.Context
	cancel  context.CancelFunc
	in      <-chan llm.Frame
	chain   []llm.ChatProvider
	idx     int
	secrets []string

	pending  []pyworker.Frame
	held     *pyworker.Frame
	errored  bool
	terminal bool
	finished bool
	frames   int
	bytes    int
	started  time.Time
}

func (s *nativeSrc) Close() { s.cancel() }

func textFrame(kind, text string) pyworker.Frame {
	d, _ := json.Marshal(text)
	return pyworker.Frame{T: kind, D: d}
}

func (s *nativeSrc) Next() (pyworker.Frame, error) {
	for len(s.pending) == 0 {
		if s.finished {
			return pyworker.Frame{}, io.EOF
		}
		var f llm.Frame
		var ok bool
		wall := time.NewTimer(max(maxRoundSeconds-time.Since(s.started), 0))
		select {
		case f, ok = <-s.in:
			wall.Stop()
		case <-wall.C:
			s.pending = append(s.pending, textFrame("error", "stream limit exceeded"))
			s.terminal, s.finished = true, true
			s.cancel()
			continue
		case <-s.ctx.Done():
			wall.Stop()
			return pyworker.Frame{}, s.ctx.Err()
		}
		if !ok {
			if err := s.ctx.Err(); err != nil {
				return pyworker.Frame{}, err
			}
			s.finish()
			continue
		}
		s.handle(f)
	}
	f := s.pending[0]
	s.pending = s.pending[1:]
	return f, nil
}

// overBudget is _Budget.exceeded for one provider frame.
func (s *nativeSrc) overBudget(size int) bool {
	s.frames++
	s.bytes += size
	return s.frames > maxRoundFrames || s.bytes > maxRoundBytes || time.Since(s.started) > maxRoundSeconds
}

func (s *nativeSrc) handle(f llm.Frame) {
	if s.terminal {
		return
	}
	size := len(f.Text)
	for _, c := range f.ToolCalls {
		size += len(c.ID) + len(c.Function.Name) + len(c.Function.Arguments)
	}
	if s.overBudget(size) {
		s.pending = append(s.pending, textFrame("error", "stream limit exceeded"))
		s.terminal, s.finished = true, true
		s.cancel()
		return
	}
	switch f.Kind {
	case llm.FrameContent, llm.FrameWarning, llm.FrameError:
		s.errored = s.errored || f.Kind == llm.FrameError
		s.pending = append(s.pending, textFrame(f.Kind, scrub(f.Text, s.secrets)))
	case llm.FrameToolCalls:
		calls := make([]llm.ToolCall, len(f.ToolCalls))
		for i, c := range f.ToolCalls {
			c.ID, c.Function.Name = scrub(c.ID, s.secrets), scrub(c.Function.Name, s.secrets)
			c.Function.Arguments = scrub(c.Function.Arguments, s.secrets)
			calls[i] = c
		}
		d, _ := json.Marshal(calls)
		s.held = &pyworker.Frame{T: "tool_calls", D: d}
	case llm.FrameDone:
		s.held = &pyworker.Frame{T: "done"}
	case llm.FrameCursor:
		n := 0
		if f.Cursor != nil {
			n = indexOf(s.chain, f.Cursor.Provider.ID)
		}
		switch {
		case s.held != nil:
			s.held.Cursor = &n
			s.pending = append(s.pending, *s.held)
			s.terminal = true
		case !s.errored:
			no := false
			s.pending = append(s.pending, pyworker.Frame{T: "done", Cursor: &n, ProviderDone: &no})
			s.terminal = true
		}
		s.held = nil
	}
}

// finish handles the end of the provider stream: a held terminal without its cursor still goes out, and a
// round that produced no terminal at all is an error.
func (s *nativeSrc) finish() {
	s.finished = true
	switch {
	case s.held != nil:
		n := s.idx
		s.held.Cursor = &n
		s.pending = append(s.pending, *s.held)
		s.held = nil
	case !s.terminal && !s.errored:
		s.pending = append(s.pending, textFrame("error", "AI provider stream ended unexpectedly"))
	}
}

// nativeRound is _llm_stream on the Go LLM client.
func (t *Turn) nativeRound(ctx context.Context, rq roundRequest) frameSource {
	rctx, cancel := context.WithCancel(ctx)
	e := t.Engine
	chain := e.chatChain(rctx)
	var secrets []string
	for _, p := range chain {
		if utf8.RuneCountInString(p.APIKey) >= minSecretLen {
			secrets = append(secrets, p.APIKey)
		}
	}
	idx := min(max(rq.cursor, 0), len(chain)-1)
	cur := llm.Cursor{Provider: chain[idx], Fallbacks: append([]llm.ChatProvider(nil), chain[idx+1:]...)}
	msgs := sanitizeMessages(rq.messages, chain[0])
	var tools []json.RawMessage
	if rq.toolsEnabled && !rq.budgetHit {
		tools = Schemas()
	} else {
		tools = []json.RawMessage{}
	}
	out := make(chan llm.Frame)
	go func() {
		defer close(out)
		send := func(f llm.Frame) bool {
			select {
			case out <- f:
				return true
			case <-rctx.Done():
				return false
			}
		}
		cur, ok := t.nonStreamingHead(rctx, cur, msgs, tools, send)
		if !ok {
			return
		}
		for f := range e.LLM.Stream(rctx, llm.StreamRequest{Messages: msgs, Tools: tools, Cursor: cur}) {
			if !send(f) {
				return
			}
		}
	}()
	return &nativeSrc{ctx: rctx, cancel: cancel, in: out, chain: chain, idx: idx, secrets: secrets, started: time.Now()}
}

// nonStreamingHead deals with a chain head that does not serve OpenAI chat completions (Python streamed it
// over /chat/completions anyway and failed): Anthropic answers through one native Messages turn, any other
// such provider is skipped. ok=false means the round is over (its frames, including the cursor, were sent).
func (t *Turn) nonStreamingHead(ctx context.Context, cur llm.Cursor, msgs []map[string]any, tools []json.RawMessage, send func(llm.Frame) bool) (llm.Cursor, bool) {
	for !cur.Provider.OpenAICompatible && cur.Provider.APIKey != "" {
		p := cur.Provider
		var next *llm.Cursor
		if len(cur.Fallbacks) > 0 {
			next = &llm.Cursor{Provider: cur.Fallbacks[0], Fallbacks: cur.Fallbacks[1:]}
		}
		if p.ID != llm.AnthropicID {
			if next == nil {
				send(llm.Frame{Kind: llm.FrameError, Text: label(p) + " cannot serve the copilot chat (it is not OpenAI-compatible). Choose another default AI provider in Settings."})
				send(llm.Frame{Kind: llm.FrameCursor, Cursor: &cur})
				return cur, false
			}
			if !send(llm.Frame{Kind: llm.FrameWarning, Text: fmt.Sprintf("⚡ %s does not support streamed chat — switching to %s...", label(p), label(next.Provider))}) {
				return cur, false
			}
			cur = *next
			continue
		}
		frames, failover := t.anthropicRound(ctx, p, msgs, tools)
		if failover != "" && next != nil {
			if !send(llm.Frame{Kind: llm.FrameWarning, Text: fmt.Sprintf("⚡ %s %s — switching to %s...", label(p), failover, label(next.Provider))}) {
				return cur, false
			}
			cur = *next
			continue
		}
		for _, f := range frames {
			if !send(f) {
				return cur, false
			}
		}
		send(llm.Frame{Kind: llm.FrameCursor, Cursor: &cur})
		return cur, false
	}
	return cur, true
}

func label(p llm.ChatProvider) string {
	if p.Name != "" {
		return p.Name
	}
	if p.ID != "" {
		return p.ID
	}
	return "unknown"
}

// anthropicRound runs one native Messages turn for the chat. It returns the round's frames (content, then
// tool_calls or done; or one error), or a non-empty failover reason when a fallback should be tried instead
// (rate limit, 5xx, rejected key, transport failure), mirroring the OpenAI streaming path.
func (t *Turn) anthropicRound(ctx context.Context, p llm.ChatProvider, msgs []map[string]any, tools []json.RawMessage) ([]llm.Frame, string) {
	system, amsgs := t.toAnthropic(msgs)
	req := llm.AnthropicRequest{
		Messages: amsgs, MaxTokens: anthropicTokens, Tools: anthropicTools(tools),
		Provider: &llm.Provider{ID: p.ID, APIKey: p.APIKey, BaseURL: p.BaseURL, Model: p.Model, Native: true},
	}
	if system != "" {
		req.System = system
	}
	resp, err := t.Engine.LLM.AnthropicTurn(ctx, req)
	if err != nil {
		var ae *llm.AnthropicError
		switch {
		case ctx.Err() != nil:
			return nil, ""
		case errors.As(err, &ae) && ae.Status == 429:
			return []llm.Frame{{Kind: llm.FrameError, Text: fmt.Sprintf("Rate limited by %s and no fallback providers available. Add more API keys in Settings, or wait and retry. (%s)", label(p), runePrefix(ae.Body, 200))}}, "rate limited"
		case errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403):
			return []llm.Frame{{Kind: llm.FrameError, Text: fmt.Sprintf("Provider error %d: %s", ae.Status, runePrefix(ae.Body, 200))}}, "rejected its API key"
		case errors.As(err, &ae) && ae.Status >= 500:
			return []llm.Frame{{Kind: llm.FrameError, Text: fmt.Sprintf("Provider error %d: %s", ae.Status, runePrefix(ae.Body, 200))}}, "error"
		case errors.As(err, &ae):
			return []llm.Frame{{Kind: llm.FrameError, Text: fmt.Sprintf("Provider error %d: %s", ae.Status, runePrefix(ae.Body, 200))}}, ""
		default:
			return []llm.Frame{{Kind: llm.FrameError, Text: "Cannot reach AI provider. Check your API key and network connection in Settings."}}, "unreachable"
		}
	}
	var frames []llm.Frame
	if txt := resp.Text(); txt != "" {
		frames = append(frames, llm.Frame{Kind: llm.FrameContent, Text: txt})
	}
	var calls []llm.ToolCall
	for _, raw := range resp.Content {
		var blk struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(raw, &blk) != nil || blk.Type != "tool_use" {
			continue
		}
		args := string(blk.Input)
		if args == "" || args == "null" {
			args = "{}"
		}
		calls = append(calls, llm.ToolCall{ID: blk.ID, Type: "function", Function: llm.ToolFunction{Name: blk.Name, Arguments: args}})
	}
	if len(calls) > 0 {
		// The next round must send this assistant turn back unchanged (thinking blocks included).
		if t.anthropicRaw == nil {
			t.anthropicRaw = map[string][]json.RawMessage{}
		}
		t.anthropicRaw[calls[0].ID] = resp.Content
		return append(frames, llm.Frame{Kind: llm.FrameToolCalls, ToolCalls: calls}), ""
	}
	return append(frames, llm.Frame{Kind: llm.FrameDone}), ""
}

// anthropicTools converts OpenAI function tools to Messages API tools.
func anthropicTools(tools []json.RawMessage) []any {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		var t struct {
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &t) != nil || t.Function.Name == "" {
			continue
		}
		params := t.Function.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]any{"name": t.Function.Name, "description": t.Function.Description, "input_schema": params})
	}
	return out
}

// toAnthropic converts the sanitized OpenAI-shaped history: leading system messages become the system prompt,
// later ones (the round-budget notice) become user text, tool messages become tool_result blocks, and an
// assistant tool-call turn this turn produced is replayed byte for byte from the stored response content.
func (t *Turn) toAnthropic(msgs []map[string]any) (string, []any) {
	var system []string
	type amsg struct {
		role   string
		blocks []any
	}
	var out []*amsg
	push := func(role string, blocks ...any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].role == role {
			out[n-1].blocks = append(out[n-1].blocks, blocks...)
			return
		}
		out = append(out, &amsg{role: role, blocks: blocks})
	}
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	for _, m := range msgs {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		switch role {
		case "system":
			if len(out) == 0 {
				system = append(system, content)
			} else if content != "" {
				push("user", text(content))
			}
		case "assistant":
			calls, _ := m["tool_calls"].([]any)
			if len(calls) == 0 {
				if strings.TrimSpace(content) != "" {
					push("assistant", text(content))
				}
				continue
			}
			first, _ := calls[0].(map[string]any)
			if id, _ := first["id"].(string); t.anthropicRaw[id] != nil {
				blocks := make([]any, len(t.anthropicRaw[id]))
				for i, b := range t.anthropicRaw[id] {
					blocks[i] = b
				}
				push("assistant", blocks...)
				continue
			}
			var blocks []any
			if strings.TrimSpace(content) != "" {
				blocks = append(blocks, text(content))
			}
			for _, c := range calls {
				cm, _ := c.(map[string]any)
				fn, _ := cm["function"].(map[string]any)
				id, _ := cm["id"].(string)
				name, _ := fn["name"].(string)
				input := map[string]any{}
				if a, _ := fn["arguments"].(string); a != "" {
					var v map[string]any
					if json.Unmarshal([]byte(a), &v) == nil && v != nil {
						input = v
					}
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
			}
			push("assistant", blocks...)
		case "tool":
			id, _ := m["tool_call_id"].(string)
			push("user", map[string]any{"type": "tool_result", "tool_use_id": id, "content": content})
		default:
			if strings.TrimSpace(content) == "" {
				content = " "
			}
			push("user", text(content))
		}
	}
	res := make([]any, len(out))
	for i, m := range out {
		res[i] = map[string]any{"role": m.role, "content": m.blocks}
	}
	return strings.Join(system, "\n\n"), res
}
