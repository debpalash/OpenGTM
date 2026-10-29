package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// ChatProvider is one provider of the copilot chat chain (OpenAI-compatible
// streaming chat completions).
type ChatProvider struct {
	ID               string
	Name             string
	APIKey           string
	BaseURL          string
	Model            string
	OpenAICompatible bool
}

// String never prints the key.
func (p ChatProvider) String() string { return "llm.ChatProvider{" + p.ID + "}" }

func (p ChatProvider) label() string {
	if p.Name != "" {
		return p.Name
	}
	if p.ID != "" {
		return p.ID
	}
	return "unknown"
}

// Cursor is the provider to call and the ordered fallbacks still available.
type Cursor struct {
	Provider  ChatProvider
	Fallbacks []ChatProvider
}

// Frame kinds emitted by Stream.
const (
	FrameContent   = "content"    // Text: a delta (also the in-band failover notice)
	FrameWarning   = "warning"    // Text: failover warning
	FrameError     = "error"      // Text: terminal error text
	FrameToolCalls = "tool_calls" // ToolCalls: complete calls ordered by index
	FrameDone      = "done"       // provider sent [DONE]
	FrameCursor    = "cursor"     // Cursor: always last
)

// ToolCall is one complete function call accumulated from the stream.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the name and raw JSON arguments of a ToolCall.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Frame is one typed event of a streamed round.
type Frame struct {
	Kind      string
	Text      string
	ToolCalls []ToolCall
	Cursor    *Cursor
}

// StreamRequest is one chat round: the messages and tool definitions are sent
// as given (OpenAI chat-completions shapes).
type StreamRequest struct {
	Messages any
	Tools    any
	Cursor   Cursor
}

// Stream performs ONE streamed chat completion, failing over across
// Cursor.Fallbacks, and reports what happened as frames (copilot/llm_stream.py
// one_round). Failover happens on 429, 5xx, 401/403 (rejected key), connection
// errors and timeouts; a tool_calls frame ends the round when the model
// finishes with tool calls. The last frame is the cursor now in effect so the
// caller can continue on whichever provider answered. The channel is closed
// after the last frame; cancelling ctx ends the round without further frames.
func (c *Client) Stream(ctx context.Context, req StreamRequest) <-chan Frame {
	out := make(chan Frame)
	go func() {
		defer close(out)
		send := func(f Frame) bool {
			select {
			case out <- f:
				return true
			case <-ctx.Done():
				return false
			}
		}
		c.streamRounds(ctx, req, send)
	}()
	return out
}

func (c *Client) streamRounds(ctx context.Context, req StreamRequest, send func(Frame) bool) {
	cursor := req.Cursor
	finish := func() { send(Frame{Kind: FrameCursor, Cursor: &cursor}) }
	for {
		prov := cursor.Provider
		if prov.APIKey == "" {
			if !send(Frame{Kind: FrameError, Text: "No AI provider configured. Go to Settings to add an API key."}) {
				return
			}
			finish()
			return
		}
		next, fin := c.streamOnce(ctx, req, cursor, send)
		if ctx.Err() != nil {
			return
		}
		if next == nil {
			if fin != nil {
				// The provider answered ([DONE] or tool calls); its cursor is final.
				send(Frame{Kind: FrameCursor, Cursor: fin})
				return
			}
			finish()
			return
		}
		cursor = *next
	}
}

type streamBody struct {
	Model       string  `json:"model"`
	Messages    any     `json:"messages"`
	Tools       any     `json:"tools"`
	Stream      bool    `json:"stream"`
	Temperature float64 `json:"temperature"`
	// Generative-UI answers embed lead data as OpenUI Lang and can be long; the
	// room keeps the DSL from being cut off mid-structure.
	MaxTokens int `json:"max_tokens"`
}

// failover returns the cursor of the next fallback, or nil when none is left.
func (cur Cursor) failover() *Cursor {
	if len(cur.Fallbacks) == 0 {
		return nil
	}
	return &Cursor{Provider: cur.Fallbacks[0], Fallbacks: cur.Fallbacks[1:]}
}

// streamOnce talks to cursor.Provider once. It returns the cursor to retry on
// (failover), or fin != nil when the round completed on this provider, or both
// nil when the round is over without a result (error frame already sent, or EOF).
func (c *Client) streamOnce(ctx context.Context, req StreamRequest, cursor Cursor, send func(Frame) bool) (next, fin *Cursor) {
	prov := cursor.Provider
	body, err := json.Marshal(streamBody{Model: prov.Model, Messages: req.Messages, Tools: req.Tools, Stream: true, Temperature: 0.7, MaxTokens: 4096})
	if err != nil {
		send(Frame{Kind: FrameError, Text: "AI provider error: " + truncRunes(err.Error(), 200)})
		return nil, nil
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timedOut atomic.Bool
	timer := time.AfterFunc(c.o.StreamTimeout, func() { timedOut.Store(true); cancel() })
	defer timer.Stop()
	// Progress resets the idle timer (httpx timeouts are per operation, not total). Only a data frame is progress:
	// a provider that sends nothing but keep-alive comments or blank lines is stalled and must still time out.
	touch := func() { timer.Reset(c.o.StreamTimeout) }
	// The stream as a whole is bounded as well (wall clock, raw lines, raw bytes and accumulated tool-call bytes):
	// a faulty or hostile provider cannot grow memory or hold the round open by trickling data.
	var overWall atomic.Bool
	wall := time.AfterFunc(maxStreamDuration, func() { overWall.Store(true); cancel() })
	defer wall.Stop()
	lines, rawBytes := 0, 0
	limit := func(send func(Frame) bool) (*Cursor, *Cursor) {
		send(Frame{Kind: FrameError, Text: "AI provider stream limit exceeded"})
		return nil, nil
	}

	hreq, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimRight(prov.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		send(Frame{Kind: FrameError, Text: "AI provider error: " + truncRunes(stripURL(err).Error(), 200)})
		return nil, nil
	}
	hreq.Header.Set("Authorization", "Bearer "+prov.APIKey)
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := c.o.HTTP.Do(hreq)
	if err != nil {
		if overWall.Load() {
			return limit(send)
		}
		return c.streamTransportError(ctx, err, timedOut.Load(), cursor, send)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		text := readErrText(resp.Body)
		if n := cursor.failover(); n != nil {
			if !send(Frame{Kind: FrameWarning, Text: fmt.Sprintf("⚡ %s rate limited — switching to %s...", prov.label(), n.Provider.label())}) {
				return nil, nil
			}
			return n, nil
		}
		send(Frame{Kind: FrameError, Text: fmt.Sprintf("Rate limited by %s and no fallback providers available. Add more API keys in Settings, or wait and retry. (%s)", prov.label(), text)})
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		text := readErrText(resp.Body)
		authRejected := resp.StatusCode == 401 || resp.StatusCode == 403
		if resp.StatusCode >= 500 || authRejected {
			if n := cursor.failover(); n != nil {
				reason := "error"
				if authRejected {
					reason = "rejected its API key"
				}
				if !send(Frame{Kind: FrameWarning, Text: fmt.Sprintf("⚡ %s %s — switching to %s...", prov.label(), reason, n.Provider.label())}) {
					return nil, nil
				}
				return n, nil
			}
		}
		send(Frame{Kind: FrameError, Text: fmt.Sprintf("Provider error %d: %s", resp.StatusCode, text)})
		return nil, nil
	}

	acc := map[int]*ToolCall{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, min(64<<10, maxStreamLine)), maxStreamLine)
	touch() // headers arrived
	for {
		more := sc.Scan()
		line := sc.Text()
		if more {
			lines++
			rawBytes += len(line) + 1
			if lines > maxStreamLines || rawBytes > maxStreamBytes || accBytes(acc) > maxStreamBytes {
				return limit(send)
			}
		}
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: "); ok && more {
			touch()
			if data == "[DONE]" {
				send(Frame{Kind: FrameDone})
				return nil, &cursor
			}
			calls, content, done := parseChunk(data, acc)
			if content != "" && !send(Frame{Kind: FrameContent, Text: content}) {
				return nil, nil
			}
			if done {
				send(Frame{Kind: FrameToolCalls, ToolCalls: calls})
				return nil, &cursor
			}
		}
		if !more {
			if overWall.Load() {
				return limit(send)
			}
			rerr := sc.Err()
			if rerr == nil {
				return nil, nil // EOF
			}
			if errors.Is(rerr, bufio.ErrTooLong) {
				send(Frame{Kind: FrameError, Text: "AI provider error: stream line too long"})
				return nil, nil
			}
			return c.streamTransportError(ctx, rerr, timedOut.Load(), cursor, send)
		}
	}
}

// maxStreamLine caps one SSE line of a chat stream and maxBatchLine one line of a batch results file; a
// longer line is an error instead of unbounded memory (variables so tests can lower them).
var (
	maxStreamLine = 4 << 20
	maxBatchLine  = 16 << 20
	// maxStreamLines, maxStreamBytes and maxStreamDuration bound one provider stream (the copilot's
	// MAX_STREAM_FRAMES / MAX_STREAM_BYTES / MAX_STREAM_SECONDS). The byte cap counts every raw line and, separately,
	// the tool-call arguments accumulated so far.
	maxStreamLines    = 20000
	maxStreamBytes    = 8 << 20
	maxStreamDuration = 300 * time.Second
)

// accBytes is the size of the tool calls accumulated so far.
func accBytes(acc map[int]*ToolCall) int {
	n := 0
	for _, c := range acc {
		n += len(c.ID) + len(c.Function.Name) + len(c.Function.Arguments)
	}
	return n
}

// parseChunk folds one SSE data payload into the tool-call accumulator. content
// is the text delta; done reports finish_reason == "tool_calls" with calls
// accumulated (calls then holds them ordered by index). Unparseable payloads
// and chunks without choices are skipped (Python failed the round on an empty
// choices list).
func parseChunk(data string, acc map[int]*ToolCall) (calls []ToolCall, content string, done bool) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      *string `json:"name"`
						Arguments *string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
		return nil, "", false
	}
	ch := chunk.Choices[0]
	for _, tc := range ch.Delta.ToolCalls {
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		cur, ok := acc[idx]
		if !ok {
			cur = &ToolCall{ID: tc.ID, Type: "function"}
			acc[idx] = cur
		}
		if tc.ID != "" {
			cur.ID = tc.ID
		}
		if tc.Function.Name != nil {
			cur.Function.Name += *tc.Function.Name
		}
		if tc.Function.Arguments != nil {
			cur.Function.Arguments += *tc.Function.Arguments
		}
	}
	if s, ok := ch.Delta.Content.(string); ok {
		content = s
	}
	if ch.FinishReason == "tool_calls" && len(acc) > 0 {
		idxs := make([]int, 0, len(acc))
		for i := range acc {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			calls = append(calls, *acc[i])
		}
		return calls, content, true
	}
	return nil, content, false
}

func readErrText(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	return truncRunes(strings.ToValidUTF8(string(b), "�"), 200)
}

// streamTransportError maps a failed request or read to frames. Connection
// failures and timeouts fail over while fallbacks remain.
func (c *Client) streamTransportError(ctx context.Context, err error, timedOut bool, cursor Cursor, send func(Frame) bool) (next, fin *Cursor) {
	if ctx.Err() != nil {
		return nil, nil
	}
	var opErr *net.OpError
	connect := errors.As(err, &opErr) && opErr.Op == "dial"
	if connect || timedOut {
		if n := cursor.failover(); n != nil {
			kind := "unreachable"
			if timedOut {
				kind = "timed out"
			}
			msg := fmt.Sprintf("\n\n> ⚡ *%s %s — switching to %s...*\n\n", cursor.Provider.label(), kind, n.Provider.label())
			if !send(Frame{Kind: FrameContent, Text: msg}) {
				return nil, nil
			}
			return n, nil
		}
		if timedOut {
			send(Frame{Kind: FrameError, Text: "AI provider timed out. The service may be overloaded — try again in a moment."})
		} else {
			send(Frame{Kind: FrameError, Text: "Cannot reach AI provider. Check your API key and network connection in Settings."})
		}
		return nil, nil
	}
	send(Frame{Kind: FrameError, Text: "AI provider error: " + truncRunes(stripURL(err).Error(), 200)})
	return nil, nil
}
