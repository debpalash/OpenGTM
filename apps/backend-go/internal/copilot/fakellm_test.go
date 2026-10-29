package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// errScriptEnded is a scripted round that ended without a terminal frame.
var errScriptEnded = errors.New("fake round: stream ended without a terminal frame")

// wireFrame is the JSON spelling of a Frame in the test scripts ({"t":"content","d":"x"}, ...).
type wireFrame struct {
	T            string          `json:"t"`
	Seq          int             `json:"seq"`
	D            json.RawMessage `json:"d"`
	Role         *string         `json:"role"`
	Content      *string         `json:"content"`
	ToolData     *string         `json:"tool_data"`
	Cursor       *int            `json:"cursor"`
	Message      string          `json:"message"`
	ProviderDone *bool           `json:"provider_done"`
}

func decodeFrame(line string) (Frame, error) {
	var w wireFrame
	if err := json.Unmarshal([]byte(line), &w); err != nil || w.T == "" {
		return Frame{}, fmt.Errorf("fake round: bad frame %q", line)
	}
	var cb bytes.Buffer
	d := w.D
	if len(d) > 0 && json.Compact(&cb, d) == nil {
		d = cb.Bytes()
	}
	return Frame{T: w.T, Seq: w.Seq, D: d, Role: w.Role, Content: w.Content, ToolData: w.ToolData, Cursor: w.Cursor,
		Message: w.Message, ProviderDone: w.ProviderDone}, nil
}

func isTerminalFrame(t string) bool { return t == "done" || t == "error" || t == "tool_calls" }

// scriptSrc replays scripted frame lines; after them it blocks until the round is closed when hold is set,
// else reports a truncated stream (no terminal frame).
type scriptSrc struct {
	ctx    context.Context
	lines  []string
	hold   bool
	closed chan struct{}
	once   sync.Once
	term   bool
}

func (s *scriptSrc) Close() { s.once.Do(func() { close(s.closed) }) }

func (s *scriptSrc) Next() (Frame, error) {
	if s.term {
		return Frame{}, io.EOF
	}
	if len(s.lines) > 0 {
		l := s.lines[0]
		s.lines = s.lines[1:]
		f, err := decodeFrame(l)
		if err != nil {
			return Frame{}, err
		}
		s.term = isTerminalFrame(f.T)
		return f, nil
	}
	if s.hold {
		select {
		case <-s.ctx.Done():
			return Frame{}, s.ctx.Err()
		case <-s.closed:
			return Frame{}, errScriptEnded
		}
	}
	return Frame{}, errScriptEnded
}

type toolReq struct {
	Body map[string]any
}

// fakeLLM scripts the llm rounds (Deps.Rounds) and records every tool call that reaches Deps.Native.
type fakeLLM struct {
	t       *testing.T
	mu      sync.Mutex
	rounds  [][]string // frame lines per llm round
	llmReqs []map[string]any
	tools   []toolReq
	// optional behaviours
	llmHold  func(n int) (lines []string, hold bool) // lines to send, then block until cancelled
	toolFn   func(req toolReq) (int, any)            // status 200 + {"result": "..."}; anything else is an error
	llmDone  chan struct{}                           // receives when a held round's context ends
	llmStart chan struct{}
}

func newFakeLLM(t *testing.T, rounds ...[]string) *fakeLLM {
	return &fakeLLM{t: t, rounds: rounds, llmDone: make(chan struct{}, 8), llmStart: make(chan struct{}, 8)}
}

// round is a RoundFunc: it records the request with the keys the worker RPC used to carry.
func (f *fakeLLM) round(ctx context.Context, rq RoundRequest) (FrameSource, error) {
	msgs, _ := json.Marshal(rq.Messages)
	var anyMsgs []any
	_ = json.Unmarshal(msgs, &anyMsgs)
	f.mu.Lock()
	n := len(f.llmReqs)
	f.llmReqs = append(f.llmReqs, map[string]any{"messages": anyMsgs, "tools_enabled": rq.ToolsEnabled,
		"provider_cursor": float64(rq.Cursor), "round_budget_hit": rq.BudgetHit})
	f.mu.Unlock()
	f.llmStart <- struct{}{}
	src := &scriptSrc{ctx: ctx, closed: make(chan struct{})}
	if f.llmHold != nil {
		if lines, hold := f.llmHold(n); hold {
			src.lines, src.hold = lines, true
			go func() {
				select {
				case <-ctx.Done():
				case <-src.closed:
				}
				f.llmDone <- struct{}{}
			}()
			return src, nil
		}
	}
	if n >= len(f.rounds) {
		return nil, errors.New("fake round: no script")
	}
	src.lines = append([]string(nil), f.rounds[n]...)
	return src, nil
}

// native is a NativeFunc standing in for the tools that once ran on the Python worker: the Go lead tools
// (nativeTools) are left to the real implementation. Every call it takes is recorded; the result is toolFn's,
// by default {"ok": true}. Like the worker's HTTP request, a call ends when ctx does.
func (f *fakeLLM) native(ctx context.Context, name string, args map[string]any, ws string, uid *int32) (string, bool, error) {
	if isNativeTool(name) {
		return "", false, nil
	}
	body := map[string]any{"name": name, "args": args, "workspace_id": ws, "user_id": nil}
	if uid != nil {
		body["user_id"] = float64(*uid)
	}
	req := toolReq{Body: body}
	f.mu.Lock()
	f.tools = append(f.tools, req)
	fn := f.toolFn
	f.mu.Unlock()
	st, out := 200, any(map[string]any{"result": `{"ok": true}`})
	if fn != nil {
		type ans struct {
			st  int
			out any
		}
		ch := make(chan ans, 1)
		go func() { s, o := fn(req); ch <- ans{s, o} }()
		select {
		case a := <-ch:
			st, out = a.st, a.out
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
	m, _ := out.(map[string]any)
	res, ok := m["result"].(string)
	if st != 200 || !ok {
		return "", false, fmt.Errorf("fake tool: status %d", st)
	}
	return res, true, nil
}

func (f *fakeLLM) llmCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return len(f.llmReqs) }
func (f *fakeLLM) toolCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.tools) }
func (f *fakeLLM) llmReq(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.llmReqs[i]
}

func runPre(t *testing.T, tn *Turn, msg string, approved bool) (handled bool, out string, err error) {
	t.Helper()
	rec := newRec()
	w, werr := ssestream.New(rec, nil)
	if werr != nil {
		t.Fatal(werr)
	}
	handled, err = tn.PreRoute(context.Background(), w, msg, approved)
	return handled, rec.String(), err
}
