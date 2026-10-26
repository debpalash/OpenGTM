//go:build integration

package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// Chaos tests (plan 12 Task 21): the Python worker dies or stalls at every frame, the client leaves at every
// frame, a tool outlives the keepalive, an approval is resubmitted concurrently, or a callback panics. In
// every case: one terminal [DONE] (or none after a disconnect), no double execution, the stream-cap slot is
// released, no goroutine of this package outlives the turn, and nothing is stored after a cancel.

const chaosSecret = "tok-secret-chaos-77"

// chaosPy is a scripted worker whose llm endpoint can cut the connection after a given frame of a given
// round, or stall (hang) after it until the request is cancelled.
type chaosPy struct {
	srv    *httptest.Server
	rounds [][]string

	mu        sync.Mutex
	cutRound  int // -1: never
	cutAfter  int // frames sent before the connection is dropped
	hangRound int // -1: never; after hangAfter frames the handler blocks until cancelled
	hangAfter int
	llmCalls  int
	toolCalls int
	toolDelay time.Duration
	cancelled atomic.Int32 // llm or tool requests whose context ended before the handler finished
}

func newChaosPy(t *testing.T, rounds ...[]string) *chaosPy {
	c := &chaosPy{rounds: rounds, cutRound: -1, hangRound: -1}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/copilot/llm", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		n := c.llmCalls
		c.llmCalls++
		cutR, cutA, hangR, hangA := c.cutRound, c.cutAfter, c.hangRound, c.hangAfter
		c.mu.Unlock()
		if n >= len(c.rounds) {
			http.Error(w, "no script", 500)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for i, l := range c.rounds[n] {
			if n == cutR && i == cutA {
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close()
					}
				}
				return
			}
			if n == hangR && i == hangA {
				<-r.Context().Done()
				c.cancelled.Add(1)
				return
			}
			_, _ = io.WriteString(w, l+"\n")
			w.(http.Flusher).Flush()
		}
		if n == hangR && hangA >= len(c.rounds[n]) {
			<-r.Context().Done()
			c.cancelled.Add(1)
		}
	})
	mux.HandleFunc("/internal/copilot/tool", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		c.toolCalls++
		d := c.toolDelay
		c.mu.Unlock()
		if d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				c.cancelled.Add(1)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": `{"ok": true}`})
	})
	mux.HandleFunc("/internal/copilot/describe", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"description": "chaos"})
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *chaosPy) client() *pyworker.Client { return pyworker.New(c.srv.URL, chaosSecret, nil) }
func (c *chaosPy) tools() int               { c.mu.Lock(); defer c.mu.Unlock(); return c.toolCalls }

// two rounds: a safe native tool call, then the answer.
func chaosRounds() [][]string {
	calls := `[{"id":"c1","type":"function","function":{"name":"search_leads","arguments":"{\"query\": \"acme\"}"}}]`
	return [][]string{
		{`{"t":"warning","d":"w"}`, `{"t":"content","d":"Let me look."}`, `{"t":"tool_calls","d":` + calls + `,"cursor":1}`},
		{`{"t":"content","d":"Found "}`, `{"t":"content","d":"it."}`, `{"t":"done","cursor":1}`},
	}
}

// leakedGoroutines lists goroutines running this module's turn, stream or SSE code (the chaos worker's
// handlers and the test goroutine itself are excluded), polling until none is left or the wait ends.
func leakedGoroutines(wait time.Duration) []string {
	deadline := time.Now().Add(wait)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		var leaked []string
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "testing.tRunner") || strings.Contains(g, "chaosPy") || strings.Contains(g, "net/http/httptest") {
				continue
			}
			if strings.Contains(g, "(*Turn)") || strings.Contains(g, "internal/pyworker.") || strings.Contains(g, "internal/ssestream.") {
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
func chaosTurn(t *testing.T, e *persistEnv, py *chaosPy, caps *ssestream.Caps, cancelAtWrite int) chaosRun {
	t.Helper()
	var out chaosRun
	var logBuf syncBuf
	var execs atomic.Int32
	tn := e.turn()
	tn.PY = py.client()
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

func TestChaosWorkerCutAtEveryFrame(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	var allOutput strings.Builder
	cases := 0
	for round := 0; round < 2; round++ {
		frames := len(chaosRounds()[round])
		for cut := 0; cut <= frames; cut++ { // cut == frames: the connection ends after the terminal frame
			e.reset(t)
			py := newChaosPy(t, chaosRounds()...)
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
	if strings.Contains(allOutput.String(), chaosSecret) {
		t.Fatal("worker bearer token leaked into a frame or log")
	}
}

func TestChaosWorkerUnavailable(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	var all strings.Builder

	// connection refused
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	// 500 and 503 from the worker
	for _, status := range []int{0, 500, 503} {
		e.reset(t)
		var srv *httptest.Server
		cl := pyworker.New(url, chaosSecret, nil)
		if status != 0 {
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "worker says "+chaosSecret, status)
			}))
			cl = pyworker.New(srv.URL, chaosSecret, nil)
		}
		tn := e.turn()
		tn.PY, tn.ToolTimeout, tn.TurnTimeout = cl, 5*time.Second, 20*time.Second
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
		if srv != nil {
			srv.Close()
		}
		body := rec.String()
		all.WriteString(body + logs.String())
		if countDone(body) != 1 || countErrs(body) != 1 {
			t.Errorf("status %d: want one error frame and one [DONE]: err=%v body=%q", status, runErr, body)
		}
		if caps.Active() != 0 {
			t.Errorf("status %d: slot leaked", status)
		}
		if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
			t.Fatalf("status %d: goroutine leak:\n%s", status, strings.Join(g, "\n\n"))
		}
		if n := e.roleCount(t, "assistant"); n != 0 {
			t.Errorf("status %d: assistant rows %d", status, n)
		}
	}
	if strings.Contains(all.String(), chaosSecret) {
		t.Fatal("secret leaked into frame or log")
	}
}

func TestChaosClientDisconnectAtEveryFrame(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	// total data frames of a clean run: warning, content, tool_call, tool_result, content, content, [DONE]
	e.reset(t)
	clean := chaosTurn(t, e, newChaosPy(t, chaosRounds()...), caps, 0)
	total := strings.Count(clean.body, "data: ")
	if total < 7 || countDone(clean.body) != 1 {
		t.Fatalf("baseline run: %q", clean.body)
	}
	for k := 1; k <= total; k++ {
		e.reset(t)
		py := newChaosPy(t, chaosRounds()...)
		r := chaosTurn(t, e, py, caps, k)
		name := fmt.Sprintf("disconnect at frame %d/%d", k, total)
		// Frames the worker had already delivered may still be written to the dead connection (Python
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

// The client leaves while the worker is mid-stream: the llm request must be cancelled (no orphaned
// provider spend), not left to run to its own deadline.
func TestChaosDisconnectCancelsWorkerRPC(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	py := newChaosPy(t, chaosRounds()...)
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
		t.Fatalf("worker llm request was not cancelled")
	}
	if caps.Active() != 0 {
		t.Fatal("slot leaked")
	}
}

func TestChaosSlowToolOutlivesKeepalive(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	calls := `[{"id":"c1","type":"function","function":{"name":"find_similar_leads","arguments":"{}"}}]`
	py := newChaosPy(t,
		[]string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`},
		[]string{`{"t":"content","d":"done"}`, `{"t":"done","cursor":0}`})
	py.toolDelay = 400 * time.Millisecond
	tn := e.turn()
	tn.PY, tn.Keepalive, tn.ToolTimeout, tn.TurnTimeout = py.client(), 50*time.Millisecond, 5*time.Second, 20*time.Second
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

func TestChaosTurnDeadlineEndsStalledWorker(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	py := newChaosPy(t, chaosRounds()...)
	py.hangRound, py.hangAfter = 0, 1
	tn := e.turn()
	tn.PY, tn.ToolTimeout, tn.TurnTimeout = py.client(), 5*time.Second, 300*time.Millisecond
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
		t.Fatalf("worker request cancelled=%d active=%d", py.cancelled.Load(), caps.Active())
	}
	if g := leakedGoroutines(3 * time.Second); len(g) != 0 {
		t.Fatalf("goroutine leak:\n%s", strings.Join(g, "\n\n"))
	}
}

func TestChaosPanicReleasesSlotAndGoroutines(t *testing.T) {
	e := persistSetup(t)
	caps := ssestream.NewCaps(4, 2)
	py := newChaosPy(t, chaosRounds()...)
	func() {
		defer func() { _ = recover() }() // the route's recover middleware
		rel, err := caps.Acquire(context.Background(), e.ws, 7)
		if err != nil {
			t.Fatal(err)
		}
		defer rel()
		tn := e.turn()
		tn.PY = py.client()
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
	py2 := newChaosPy(t, chaosRounds()...)
	tn := e.turn()
	tn.PY = py2.client()
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

// The client leaves while an approved tool runs: the worker request is cancelled, the approval stays
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

// The worker dies while an approved tool runs (connection refused): the turn reports a tool failure, the
// approval is consumed, nothing is retried.
func TestChaosWorkerDownDuringApprovedTool(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{"name": "x"}`)
	e.py.srv.Close()
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
	if err != nil {
		t.Logf("resolve error (acceptable, fatal for the turn): %v", err)
	}
	if strings.Contains(body, `"ok": true`) {
		t.Fatalf("tool cannot have succeeded: %q", body)
	}
	if strings.Contains(body, chaosSecret) || strings.Contains(body, "tok-secret-9f3") {
		t.Fatal("secret in frames")
	}
	msgs = nil
	again, _ := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
	if !strings.Contains(again, `{"tool_denied": {"name": "invalid_approval"}}`) {
		t.Fatalf("second resubmit must be denied: %q", again)
	}
}
