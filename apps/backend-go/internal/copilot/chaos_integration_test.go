//go:build integration

package copilot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// Chaos tests (plan 12 Task 21): the llm round dies or stalls at every frame, the client leaves at every
// frame, a tool outlives the keepalive, an approval is resubmitted concurrently, or a callback panics. In
// every case: one terminal [DONE] (or none after a disconnect), no double execution, the stream-cap slot is
// released, no goroutine of this package outlives the turn, and nothing is stored after a cancel.

// chaosLLM scripts the llm rounds; a round can fail after a given frame of a given round (the provider stream
// breaks), or stall (hang) after it until the turn cancels it. Its native tool can be slowed down.
type chaosLLM struct {
	rounds [][]string

	mu        sync.Mutex
	cutRound  int // -1: never
	cutAfter  int // frames delivered before the stream breaks
	hangRound int // -1: never; after hangAfter frames the round blocks until cancelled
	hangAfter int
	openErr   error // every round fails to open
	llmCalls  int
	toolCalls int
	toolDelay time.Duration
	cancelled atomic.Int32 // rounds or tools whose context ended before they finished
}

func newChaosLLM(rounds ...[]string) *chaosLLM {
	return &chaosLLM{rounds: rounds, cutRound: -1, hangRound: -1}
}

var errChaosCut = errors.New("chaos: provider stream broke")

type chaosSrc struct {
	c      *chaosLLM
	ctx    context.Context
	lines  []string
	sent   int
	cut    int // -1: never
	hang   int // -1: never
	closed chan struct{}
	once   sync.Once
}

func (s *chaosSrc) Close() { s.once.Do(func() { close(s.closed) }) }

func (s *chaosSrc) Next() (Frame, error) {
	if s.sent == s.cut {
		return Frame{}, errChaosCut
	}
	if s.sent == s.hang {
		select {
		case <-s.ctx.Done():
			s.c.cancelled.Add(1)
			return Frame{}, s.ctx.Err()
		case <-s.closed:
			s.c.cancelled.Add(1)
			return Frame{}, errScriptEnded
		}
	}
	if s.sent >= len(s.lines) {
		return Frame{}, errScriptEnded
	}
	l := s.lines[s.sent]
	s.sent++
	return decodeFrame(l)
}

func (c *chaosLLM) round(ctx context.Context, _ RoundRequest) (FrameSource, error) {
	c.mu.Lock()
	n := c.llmCalls
	c.llmCalls++
	src := &chaosSrc{c: c, ctx: ctx, cut: -1, hang: -1, closed: make(chan struct{})}
	if n == c.cutRound {
		src.cut = c.cutAfter
	}
	if n == c.hangRound {
		src.hang = c.hangAfter
	}
	openErr := c.openErr
	c.mu.Unlock()
	if openErr != nil {
		return nil, openErr
	}
	if n >= len(c.rounds) {
		return nil, errors.New("chaos: no script")
	}
	src.lines = c.rounds[n]
	return src, nil
}

func (c *chaosLLM) native(ctx context.Context, _ string, _ map[string]any, _ string, _ *int32) (string, bool, error) {
	c.mu.Lock()
	c.toolCalls++
	d := c.toolDelay
	c.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			c.cancelled.Add(1)
			return "", false, ctx.Err()
		}
	}
	return `{"ok": true}`, true, nil
}

func (c *chaosLLM) tools() int { c.mu.Lock(); defer c.mu.Unlock(); return c.toolCalls }

// two rounds: a safe native tool call, then the answer.
func chaosRounds() [][]string {
	calls := `[{"id":"c1","type":"function","function":{"name":"search_leads","arguments":"{\"query\": \"acme\"}"}}]`
	return [][]string{
		{`{"t":"warning","d":"w"}`, `{"t":"content","d":"Let me look."}`, `{"t":"tool_calls","d":` + calls + `,"cursor":1}`},
		{`{"t":"content","d":"Found "}`, `{"t":"content","d":"it."}`, `{"t":"done","cursor":1}`},
	}
}

// leakedGoroutines lists goroutines running this module's turn, stream or SSE code (the test goroutine itself is
// excluded), polling until none is left or the wait ends.
func leakedGoroutines(wait time.Duration) []string {
	deadline := time.Now().Add(wait)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		var leaked []string
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "testing.tRunner") {
				continue
			}
			if strings.Contains(g, "(*Turn)") || strings.Contains(g, "(*chaosSrc)") || strings.Contains(g, "internal/ssestream.") {
				leaked = append(leaked, g)
			}
		}
		if len(leaked) == 0 || time.Now().After(deadline) {
			return leaked
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type chaosRun struct {
	body string
	err  error
	logs string
	exec int32 // native tool executions
}

// run drives one turn the way the chat route does: slot first (released by defer), then the turn under
// CaptureAndStore so persistence is exercised. cancelAtWrite > 0 cancels the client context when that many
// data frames have been written.
func chaosTurn(t *testing.T, e *persistEnv, py *chaosLLM, caps *ssestream.Caps, cancelAtWrite int) chaosRun {
	t.Helper()
	var out chaosRun
	var logBuf syncBuf
	var execs atomic.Int32
	tn := e.turn()
	tn.Rounds = py.round
	tn.ToolTimeout, tn.TurnTimeout = 5*time.Second, 20*time.Second
	tn.Log = slog.New(slog.NewTextHandler(&logBuf, nil))
	tn.Native = func(context.Context, string, map[string]any, string, *int32) (string, bool, error) {
		execs.Add(1)
		return `{"leads": [], "count": 0}`, true, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rel, err := caps.Acquire(ctx, e.ws, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	rec := newRec()
	var writes atomic.Int32
	rw := &countingRW{syncRec: rec, onData: func() {
		if cancelAtWrite > 0 && int(writes.Add(1)) == cancelAtWrite {
			cancel()
		}
	}}
	w, err := ssestream.New(rw, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		out.err = tn.CaptureAndStore(ctx, "find acme", func(ctx context.Context) error {
			return tn.Run(ctx, w, userMsgs())
		})
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("turn hung")
	}
	out.body, out.logs, out.exec = rec.String(), logBuf.String(), execs.Load()
	return out
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// countingRW calls onData after every "data: " write (keepalive comments are not counted).
type countingRW struct {
	*syncRec
	onData func()
}

func (c *countingRW) Write(b []byte) (int, error) {
	n, err := c.syncRec.Write(b)
	if bytes.HasPrefix(b, []byte("data: ")) {
		c.onData()
	}
	return n, err
}

func countDone(body string) int { return strings.Count(body, "data: [DONE]\n\n") }
func countErrs(body string) int { return strings.Count(body, `data: {"error": `) }

func (e *persistEnv) roleCount(t *testing.T, role string) int {
	n := 0
	for _, m := range e.messages(t) {
		if m.Role == role {
			n++
		}
	}
	return n
}

func (e *persistEnv) reset(t *testing.T) {
	t.Helper()
	_, _ = e.owner.Exec(context.Background(), `DELETE FROM chat_messages WHERE workspace_id = $1`, e.ws)
	_, _ = e.owner.Exec(context.Background(), `DELETE FROM chat_memories WHERE user_id LIKE $1`, e.ws+":%")
}

func TestChaosRoundCutAtEveryFrame(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	var allOutput strings.Builder
	cases := 0
	for round := 0; round < 2; round++ {
		frames := len(chaosRounds()[round])
		for cut := 0; cut <= frames; cut++ { // cut == frames: the stream breaks after the terminal frame
			e.reset(t)
			py := newChaosLLM(chaosRounds()...)
			py.cutRound, py.cutAfter = round, cut
			r := chaosTurn(t, e, py, caps, 0)
			allOutput.WriteString(r.body + r.logs)
			cases++
			name := fmt.Sprintf("round %d cut after %d frames", round, cut)

			if cut == frames {
				// Round 0 ends with tool_calls (terminal for the round) and round 1 with done: not a fault.
				if countErrs(r.body) != 0 || countDone(r.body) != 1 || r.err != nil {
					t.Errorf("%s: clean end expected: err=%v body=%q", name, r.err, r.body)
				}
			} else {
				if countDone(r.body) != 1 || !strings.HasSuffix(r.body, "data: [DONE]\n\n") {
					t.Errorf("%s: want exactly one terminal [DONE]: %q", name, r.body)
				}
				if countErrs(r.body) != 1 {
					t.Errorf("%s: want exactly one error frame: %q", name, r.body)
				}
			}
			wantExec := int32(0)
			if round == 1 || cut == frames {
				wantExec = 1
			}
			if r.exec != wantExec {
				t.Errorf("%s: tool executed %d times, want %d", name, r.exec, wantExec)
			}
			if caps.Active() != 0 {
				t.Errorf("%s: cap slot leaked: %d", name, caps.Active())
			}
			if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
				t.Fatalf("%s: goroutine leak:\n%s", name, strings.Join(g, "\n\n"))
			}
			// Persistence after an error frame stores only what was streamed: never an assistant row
			// for text that was not sent.
			if rows := e.roleCount(t, "assistant"); rows > 1 {
				t.Errorf("%s: %d assistant rows", name, rows)
			}
		}
	}
	if cases != 4+4 {
		t.Fatalf("cases %d", cases)
	}
	if strings.Contains(allOutput.String(), errChaosCut.Error()) {
		t.Fatal("the provider failure text leaked into a frame")
	}
}

func TestChaosRoundUnavailable(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	for i, py := range []*chaosLLM{
		{openErr: errors.New("chaos: provider unreachable"), cutRound: -1, hangRound: -1}, // the round cannot open
		{rounds: [][]string{{}}, cutRound: 0, cutAfter: 0, hangRound: -1},                 // it breaks before any frame
	} {
		e.reset(t)
		tn := e.turn()
		tn.Rounds, tn.ToolTimeout, tn.TurnTimeout = py.round, 5*time.Second, 20*time.Second
		var logs syncBuf
		tn.Log = slog.New(slog.NewTextHandler(&logs, nil))
		rel, err := caps.Acquire(context.Background(), e.ws, 7)
		if err != nil {
			t.Fatal(err)
		}
		rec := newRec()
		w, _ := ssestream.New(rec, nil)
		runErr := tn.CaptureAndStore(context.Background(), "hi", func(ctx context.Context) error { return tn.Run(ctx, w, userMsgs()) })
		rel()
		body := rec.String()
		if countDone(body) != 1 || countErrs(body) != 1 || strings.Contains(body, "chaos:") {
			t.Errorf("case %d: want one fixed error frame and one [DONE]: err=%v body=%q", i, runErr, body)
		}
		if caps.Active() != 0 {
			t.Errorf("case %d: slot leaked", i)
		}
		if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
			t.Fatalf("case %d: goroutine leak:\n%s", i, strings.Join(g, "\n\n"))
		}
		if n := e.roleCount(t, "assistant"); n != 0 {
			t.Errorf("case %d: assistant rows %d", i, n)
		}
	}
}

func TestChaosClientDisconnectAtEveryFrame(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	// total data frames of a clean run: warning, content, tool_call, tool_result, content, content, [DONE]
	e.reset(t)
	clean := chaosTurn(t, e, newChaosLLM(chaosRounds()...), caps, 0)
	total := strings.Count(clean.body, "data: ")
	if total < 7 || countDone(clean.body) != 1 {
		t.Fatalf("baseline run: %q", clean.body)
	}
	for k := 1; k <= total; k++ {
		e.reset(t)
		py := newChaosLLM(chaosRounds()...)
		r := chaosTurn(t, e, py, caps, k)
		name := fmt.Sprintf("disconnect at frame %d/%d", k, total)
		// Frames the round had already delivered may still be written to the dead connection (Python
		// cancels at the next yield; Go drains what the stream buffered), but never a second [DONE], and
		// nothing is stored (checked below).
		if countDone(r.body) > 1 {
			t.Errorf("%s: more than one [DONE]: %q", name, r.body)
		}
		if r.exec > 1 {
			t.Errorf("%s: tool executed %d times", name, r.exec)
		}
		if caps.Active() != 0 {
			t.Errorf("%s: slot leaked", name)
		}
		if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
			t.Fatalf("%s: goroutine leak:\n%s", name, strings.Join(g, "\n\n"))
		}
		// Rows written after the cancel: none. The tool row is stored just before frame 4 is written, so it
		// exists exactly when the client was still there for that frame.
		wantTool := 0
		if k >= 4 {
			wantTool = 1
		}
		if n := e.roleCount(t, "tool"); n != wantTool {
			t.Errorf("%s: tool rows %d, want %d", name, n, wantTool)
		}
		// Python stores nothing after a disconnect: no assistant row, no memory.
		if k < total {
			if n := e.roleCount(t, "assistant"); n != 0 {
				t.Errorf("%s: assistant row written after cancel", name)
			}
			if m := e.memories(t); len(m) != 0 {
				t.Errorf("%s: memory written after cancel", name)
			}
		}
	}
}

// The client leaves while the provider is mid-stream: the round must be cancelled (no orphaned provider
// spend), not left to run to its own deadline.
func TestChaosDisconnectCancelsRound(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	py := newChaosLLM(chaosRounds()...)
	py.hangRound, py.hangAfter = 0, 2 // after warning+content the provider stalls
	// cancel when the 2nd data frame is written
	r := chaosTurn(t, e, py, caps, 2)
	if r.err == nil || countDone(r.body) != 0 {
		t.Fatalf("err=%v body=%q", r.err, r.body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for py.cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if py.cancelled.Load() != 1 {
		t.Fatalf("the llm round was not cancelled")
	}
	if caps.Active() != 0 {
		t.Fatal("slot leaked")
	}
}

func TestChaosSlowToolOutlivesKeepalive(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	calls := `[{"id":"c1","type":"function","function":{"name":"find_similar_leads","arguments":"{}"}}]`
	py := newChaosLLM(
		[]string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`},
		[]string{`{"t":"content","d":"done"}`, `{"t":"done","cursor":0}`})
	py.toolDelay = 400 * time.Millisecond
	tn := e.turn()
	tn.Rounds, tn.Native, tn.Keepalive, tn.ToolTimeout, tn.TurnTimeout = py.round, py.native, 50*time.Millisecond, 5*time.Second, 20*time.Second
	rel, err := caps.Acquire(context.Background(), e.ws, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	if err := tn.Run(context.Background(), w, userMsgs()); err != nil {
		t.Fatal(err)
	}
	body := rec.String()
	if n := strings.Count(body, ": keepalive\n\n"); n < 3 {
		t.Fatalf("keepalives %d: %q", n, body)
	}
	if countDone(body) != 1 || py.tools() != 1 {
		t.Fatalf("done=%d tool executions=%d body=%q", countDone(body), py.tools(), body)
	}
	if i, j := strings.Index(body, ": keepalive"), strings.Index(body, `{"tool_result"`); i < 0 || j < i {
		t.Fatalf("keepalive must precede the result: %q", body)
	}
}

func TestChaosTurnDeadlineEndsStalledRound(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	py := newChaosLLM(chaosRounds()...)
	py.hangRound, py.hangAfter = 0, 1
	tn := e.turn()
	tn.Rounds, tn.ToolTimeout, tn.TurnTimeout = py.round, 5*time.Second, 300*time.Millisecond
	rel, _ := caps.Acquire(context.Background(), e.ws, 7)
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	_ = tn.Run(context.Background(), w, userMsgs())
	rel()
	body := rec.String()
	if countErrs(body) != 1 || countDone(body) != 1 {
		t.Fatalf("deadline must end with one error frame and [DONE]: %q", body)
	}
	if py.cancelled.Load() != 1 || caps.Active() != 0 {
		t.Fatalf("round cancelled=%d active=%d", py.cancelled.Load(), caps.Active())
	}
	if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
		t.Fatalf("goroutine leak:\n%s", strings.Join(g, "\n\n"))
	}
}

func TestChaosPanicReleasesSlotAndGoroutines(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	py := newChaosLLM(chaosRounds()...)
	func() {
		defer func() { _ = recover() }() // the route's recover middleware
		rel, err := caps.Acquire(context.Background(), e.ws, 7)
		if err != nil {
			t.Fatal(err)
		}
		defer rel()
		tn := e.turn()
		tn.Rounds = py.round
		tn.OnEvent = func(raw []byte) {
			if bytes.Contains(raw, []byte("Let me look")) {
				panic("boom")
			}
		}
		w, _ := ssestream.New(newRec(), nil)
		_ = tn.Run(context.Background(), w, userMsgs())
	}()
	if caps.Active() != 0 {
		t.Fatalf("slot leaked after panic: %d", caps.Active())
	}
	if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
		t.Fatalf("goroutine leak after panic:\n%s", strings.Join(g, "\n\n"))
	}
	// a panicking native tool is contained inside the turn (await recovers) and reported as a tool error
	py2 := newChaosLLM(chaosRounds()...)
	tn := e.turn()
	tn.Rounds = py2.round
	tn.Native = func(context.Context, string, map[string]any, string, *int32) (string, bool, error) {
		panic("tool boom")
	}
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	if err := tn.Run(context.Background(), w, userMsgs()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.String(), "failed") || countDone(rec.String()) != 1 {
		t.Fatalf("tool panic must become a tool error and the turn must finish: %q", rec.String())
	}
	if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
		t.Fatalf("goroutine leak after tool panic:\n%s", strings.Join(g, "\n\n"))
	}
}

// Many concurrent resubmits of one approval execute the tool exactly once; the losers see the denial.
func TestChaosConcurrentApprovalResubmit(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{"name": "x"}`)
	const n = 12
	var wg sync.WaitGroup
	bodies := make([]string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			msgs := []map[string]any{}
			b, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
			if err != nil {
				t.Error(err)
			}
			bodies[i] = b
		}()
	}
	close(start)
	wg.Wait()
	if c := e.py.toolCount(); c != 1 {
		t.Fatalf("executions = %d, want exactly 1", c)
	}
	exec, denied := 0, 0
	for _, b := range bodies {
		if strings.Contains(b, `{"tool_call": {"name": "create_people_workbook"`) {
			exec++
		}
		if strings.Contains(b, `{"tool_denied": {"name": "invalid_approval"}}`) {
			denied++
		}
	}
	if exec != 1 || denied != n-1 {
		t.Fatalf("exec=%d denied=%d", exec, denied)
	}
	if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
		t.Fatalf("goroutine leak:\n%s", strings.Join(g, "\n\n"))
	}
}

// The client leaves while an approved tool runs: the tool is cancelled, the approval stays
// consumed (documented deviation) and a resubmit never runs the tool a second time.
func TestChaosCancelDuringApprovedToolThenResubmit(t *testing.T) {
	e := resumeSetup(t)
	started := make(chan struct{}, 1)
	e.py.toolFn = func(req toolReq) (int, any) {
		started <- struct{}{}
		time.Sleep(300 * time.Millisecond)
		return 200, map[string]any{"result": `{"ok": true}`}
	}
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{"name": "x"}`)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		rec := newRec()
		w, _ := ssestream.New(rec, nil)
		msgs := []map[string]any{}
		errc <- e.turn(1).ResolveApproved(ctx, w, &msgs, []Approved{approve(id)})
	}()
	<-started
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("cancelled resolve returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolve hung after cancel")
	}
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `{"tool_denied": {"name": "invalid_approval"}}`) {
		t.Fatalf("resubmit after a cancelled turn must be denied (approval consumed): %q", body)
	}
	if c := e.py.toolCount(); c != 1 {
		t.Fatalf("tool ran %d times", c)
	}
	if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
		t.Fatalf("goroutine leak:\n%s", strings.Join(g, "\n\n"))
	}
}

// The approved tool fails (its backend is down): the turn reports a tool failure, the approval is consumed,
// nothing is retried.
func TestChaosToolFailsDuringApprovedTool(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{"name": "x"}`)
	e.py.toolFn = func(toolReq) (int, any) { return 503, map[string]any{"detail": "backend down tok-secret-9f3"} }
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
	if err != nil {
		t.Logf("resolve error (acceptable, fatal for the turn): %v", err)
	}
	if strings.Contains(body, `"ok": true`) || strings.Contains(body, "tok-secret-9f3") {
		t.Fatalf("tool cannot have succeeded or leaked: %q", body)
	}
	msgs = nil
	again, _ := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
	if !strings.Contains(again, `{"tool_denied": {"name": "invalid_approval"}}`) {
		t.Fatalf("second resubmit must be denied: %q", again)
	}
	if e.py.toolCount() != 1 {
		t.Fatalf("tool ran %d times", e.py.toolCount())
	}
}
