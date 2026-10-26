package pyworker

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ndjson(lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n"))
		}
	}
}

func drain(s *Stream) ([]Frame, error) {
	var out []Frame
	for {
		f, err := s.Next()
		if err != nil {
			return out, err
		}
		out = append(out, f)
	}
}

func TestStreamReadsFramesInOrder(t *testing.T) {
	var auth, path, ctype atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		path.Store(r.URL.Path)
		ctype.Store(r.Header.Get("Content-Type"))
		ndjson(`{"t":"sse","seq":1,"d":{"a":"é"}}`,
			`{"t":"persist","seq":2,"role":"tool","content":"c","tool_data":"{}"}`,
			``,
			`{"t":"content","d":"x"}`,
			`{"t":"unknown_future","x":1}`,
			`{"t":"error","message":"boom"}`)(w, r)
	}))
	defer srv.Close()
	s, err := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", map[string]any{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fr, err := drain(s)
	if err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if len(fr) != 5 || fr[0].T != "sse" || fr[0].Seq != 1 || string(fr[0].D) != `{"a":"é"}` {
		t.Fatalf("frames %+v", fr)
	}
	if fr[1].Role == nil || *fr[1].Role != "tool" || *fr[1].Content != "c" || *fr[1].ToolData != "{}" {
		t.Fatalf("persist %+v", fr[1])
	}
	if fr[2].T != "content" || fr[3].T != "unknown_future" || fr[4].Message != "boom" {
		t.Fatalf("frames %+v", fr)
	}
	if auth.Load() != "Bearer "+testToken || path.Load() != "/internal/copilot/llm" || ctype.Load() != "application/json" {
		t.Fatal("request headers/path wrong")
	}
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("EOF must be sticky, got %v", err)
	}
}

func TestStreamTerminalNoneAndDone(t *testing.T) {
	for _, term := range []string{"none", "done"} {
		srv := httptest.NewServer(ndjson(`{"t":"` + term + `"}`))
		s, err := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/preroute", struct{}{})
		if err != nil {
			t.Fatal(err)
		}
		fr, err := drain(s)
		if err != io.EOF || len(fr) != 1 || fr[0].T != term {
			t.Fatalf("%s: %v %+v", term, err, fr)
		}
		s.Close()
		srv.Close()
	}
}

func TestStreamTruncationIsError(t *testing.T) {
	for name, lines := range map[string][]string{
		"no terminal": {`{"t":"sse","seq":1,"d":1}`},
		"empty":       {},
	} {
		srv := httptest.NewServer(ndjson(lines...))
		s, _ := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
		_, err := drain(s)
		if !errors.Is(err, ErrTruncated) {
			t.Fatalf("%s: want ErrTruncated, got %v", name, err)
		}
		s.Close()
		srv.Close()
	}
	// partial last line without newline
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"t":"done"`))
	}))
	defer srv.Close()
	s, _ := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	if _, err := drain(s); !errors.Is(err, ErrTruncated) {
		t.Fatalf("partial line: %v", err)
	}
}

func TestStreamSeqRegressionIsProtocolError(t *testing.T) {
	for _, lines := range [][]string{
		{`{"t":"sse","seq":2,"d":1}`, `{"t":"sse","seq":2,"d":1}`, `{"t":"done"}`},
		{`{"t":"sse","seq":3,"d":1}`, `{"t":"sse","seq":1,"d":1}`, `{"t":"done"}`},
	} {
		srv := httptest.NewServer(ndjson(lines...))
		s, _ := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
		fr, err := drain(s)
		if !errors.Is(err, ErrProtocol) || len(fr) != 1 {
			t.Fatalf("want ErrProtocol after 1 frame, got %v %d", err, len(fr))
		}
		if _, err2 := s.Next(); !errors.Is(err2, ErrProtocol) {
			t.Fatalf("error must be sticky: %v", err2)
		}
		srv.Close()
	}
}

func TestStreamMalformedFrameIsProtocolError(t *testing.T) {
	for _, l := range []string{`not json`, `[1]`, `{"seq":1}`, `{"t":5}`} {
		srv := httptest.NewServer(ndjson(l, `{"t":"done"}`))
		s, _ := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
		if _, err := drain(s); !errors.Is(err, ErrProtocol) {
			t.Fatalf("%q: %v", l, err)
		}
		srv.Close()
	}
}

func TestStreamOversizedFrame(t *testing.T) {
	big := `{"t":"sse","d":"` + strings.Repeat("a", maxFrameBytes+10) + `"}`
	srv := httptest.NewServer(ndjson(big, `{"t":"done"}`))
	defer srv.Close()
	s, _ := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	if _, err := drain(s); !errors.Is(err, ErrProtocol) {
		t.Fatalf("want ErrProtocol, got %v", err)
	}
	// a frame just under the cap passes
	ok := `{"t":"sse","d":"` + strings.Repeat("a", maxFrameBytes-100) + `"}`
	srv2 := httptest.NewServer(ndjson(ok, `{"t":"done"}`))
	defer srv2.Close()
	s2, _ := New(srv2.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	if fr, err := drain(s2); err != io.EOF || len(fr) != 2 {
		t.Fatalf("%v %d", err, len(fr))
	}
}

func TestStreamFrameCountAndTotalBytesCaps(t *testing.T) {
	c := New("", "", nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 1; i <= 50; i++ {
			_, _ = w.Write([]byte(`{"t":"sse","d":"xxxxxxxxxx"}` + "\n"))
		}
		_, _ = w.Write([]byte(`{"t":"done"}` + "\n"))
	}))
	defer srv.Close()
	c = New(srv.URL, testToken, nil)
	c.maxFrames = 10
	s, _ := c.Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	if _, err := drain(s); !errors.Is(err, ErrProtocol) {
		t.Fatalf("frame cap: %v", err)
	}
	c.maxFrames = 1000
	c.maxTotalBytes = 200
	s, _ = c.Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	if _, err := drain(s); !errors.Is(err, ErrProtocol) {
		t.Fatalf("byte cap: %v", err)
	}
}

func TestStreamCloseCancelsServerRequest(t *testing.T) {
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"t":"sse","seq":1,"d":1}` + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()
	s, err := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close() // idempotent
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not observe cancellation")
	}
	if _, err := s.Next(); err == nil {
		t.Fatal("Next after Close must fail")
	}
}

func TestStreamDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"t":"sse","seq":1,"d":1}` + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	s, err := New(srv.URL, testToken, nil).Stream(ctx, "/internal/copilot/llm", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	_, err = s.Next()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestStreamHTTPStatusMapping(t *testing.T) {
	cases := map[int]error{401: ErrUnauthorized, 503: ErrUnauthorized, 404: ErrUnknownProvider, 408: ErrTimeout}
	for st, want := range cases {
		st := st
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"detail":"` + testToken + `"}`))
		}))
		_, err := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
		if !errors.Is(err, want) {
			t.Fatalf("%d: %v", st, err)
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	_, err := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("500: %v", err)
	}
	// redirects are not followed
	var hits atomic.Int32
	tgt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer tgt.Close()
	rd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, tgt.URL, 307) }))
	defer rd.Close()
	if _, err := New(rd.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{}); err == nil || hits.Load() != 0 {
		t.Fatalf("redirect followed or accepted: %v", err)
	}
}

func TestStreamUnavailableAndBadPath(t *testing.T) {
	if _, err := New("", "", nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	c := New("http://127.0.0.1:1", testToken, nil)
	if _, err := c.Stream(context.Background(), "//evil.example/x", struct{}{}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("bad path: %v", err)
	}
}

func TestUnaryRetriesOnceWithSameCallID(t *testing.T) {
	var ids []string
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get("X-Call-Id"))
		if n.Add(1) == 1 {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close() // connection error before any response
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New(srv.URL, testToken, nil)
	c.backoff = time.Millisecond
	var out struct{ OK bool }
	if err := c.Unary(context.Background(), "/internal/copilot/tool", map[string]any{"a": 1}, &out, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if !out.OK || len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("ids %v out %+v", ids, out)
	}
}

func TestUnaryNoRetryAfterResponseStarted(t *testing.T) {
	for _, st := range []int{200, 500} {
		st := st
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(st)
			w.(http.Flusher).Flush()
			if st == 200 {
				_, _ = w.Write([]byte(`{"ok":`)) // then die mid-body
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				conn.Close()
			}
		}))
		c := New(srv.URL, testToken, nil)
		c.backoff = time.Millisecond
		var out map[string]any
		if err := c.Unary(context.Background(), "/internal/copilot/tool", struct{}{}, &out, 5*time.Second); err == nil {
			t.Fatalf("%d: expected error", st)
		}
		if n.Load() != 1 {
			t.Fatalf("%d: retried after response started (%d calls)", st, n.Load())
		}
		srv.Close()
	}
}

func TestUnaryStatusAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/copilot/t408":
			w.WriteHeader(408)
		case "/internal/copilot/t401":
			w.WriteHeader(401)
		case "/internal/copilot/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
	}))
	defer srv.Close()
	c := New(srv.URL, testToken, nil)
	if err := c.Unary(context.Background(), "/internal/copilot/t408", struct{}{}, nil, time.Second); !errors.Is(err, ErrTimeout) {
		t.Fatalf("408: %v", err)
	}
	if err := c.Unary(context.Background(), "/internal/copilot/t401", struct{}{}, nil, time.Second); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401: %v", err)
	}
	if err := c.Unary(context.Background(), "/internal/copilot/slow", struct{}{}, nil, 100*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("slow: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Unary(ctx, "/internal/copilot/slow", struct{}{}, nil, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestErrorsNeverContainToken(t *testing.T) {
	const body = "SECRET-BODY-VALUE"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(testToken + body))
	}))
	url := srv.URL
	srv.Close() // connection refused; url.Error would carry the URL
	check := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error")
		}
		if s := err.Error(); strings.Contains(s, testToken) || strings.Contains(s, body) {
			t.Fatalf("leak: %q", s)
		}
	}
	c := New(url, testToken, nil)
	c.backoff = time.Millisecond
	_, err := c.Stream(context.Background(), "/internal/copilot/llm", map[string]any{"k": body})
	check(err)
	check(c.Unary(context.Background(), "/internal/copilot/tool", map[string]any{"k": body}, nil, time.Second))

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(testToken + body))
	}))
	defer srv2.Close()
	c2 := New(srv2.URL, testToken, nil)
	_, err = c2.Stream(context.Background(), "/internal/copilot/llm", map[string]any{"k": body})
	check(err)
	check(c2.Unary(context.Background(), "/internal/copilot/tool", map[string]any{"k": body}, nil, time.Second))
}

func TestNoGoroutineLeakAfterClose(t *testing.T) {
	tr := &http.Transport{}
	hc := &http.Client{Transport: tr}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"t":"sse","seq":1,"d":1}` + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := New(srv.URL, testToken, hc)
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		s, err := c.Stream(context.Background(), "/internal/copilot/llm", struct{}{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = s.Next()
		s.Close()
	}
	tr.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before+2 {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}

func TestStreamToolCallsIsTerminalWithCursor(t *testing.T) {
	srv := httptest.NewServer(ndjson(`{"t":"content","d":"a"}`, `{"t":"tool_calls","d":[{"id":"c"}],"cursor":2}`))
	defer srv.Close()
	s, _ := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	defer s.Close()
	fr, err := drain(s)
	if err != io.EOF || len(fr) != 2 || fr[1].T != "tool_calls" || fr[1].Cursor == nil || *fr[1].Cursor != 2 {
		t.Fatalf("%v %+v", err, fr)
	}
}

func TestStreamProviderDone(t *testing.T) {
	srv := httptest.NewServer(ndjson(`{"t":"done","cursor":1,"provider_done":false}`))
	defer srv.Close()
	s, _ := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	fr, _ := drain(s)
	if len(fr) != 1 || fr[0].ProviderDone == nil || *fr[0].ProviderDone {
		t.Fatalf("%+v", fr)
	}
	srv2 := httptest.NewServer(ndjson(`{"t":"done","cursor":1}`))
	defer srv2.Close()
	s2, _ := New(srv2.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	fr, _ = drain(s2)
	if len(fr) != 1 || fr[0].ProviderDone != nil {
		t.Fatalf("%+v", fr)
	}
}

func TestStreamRecordedFrames(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/copilot/frames/*.ndjson")
	if len(files) == 0 {
		t.Fatal("no recorded frame files")
	}
	checked := 0
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var rec struct {
				RPC    string            `json:"rpc"`
				Status int               `json:"status"`
				Frames []json.RawMessage `json:"frames"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Status != 200 || len(rec.Frames) == 0 || (rec.RPC != "llm" && rec.RPC != "preroute") {
				continue
			}
			lines := make([]string, len(rec.Frames))
			for i, fr := range rec.Frames {
				lines[i] = string(fr)
			}
			srv := httptest.NewServer(ndjson(lines...))
			s, err := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/"+rec.RPC, struct{}{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := drain(s)
			s.Close()
			srv.Close()
			if err != io.EOF || len(got) != len(rec.Frames) {
				t.Fatalf("%s %s: err=%v got %d want %d", f, rec.RPC, err, len(got), len(rec.Frames))
			}
			checked++
		}
		fh.Close()
	}
	if checked < 10 {
		t.Fatalf("only %d recorded streams checked", checked)
	}
}

func TestStreamEightMiBFrameAccepted(t *testing.T) {
	line := `{"t":"content","d":"` + strings.Repeat("a", 8<<20) + `"}`
	srv := httptest.NewServer(ndjson(line, `{"t":"done"}`))
	defer srv.Close()
	s, _ := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	defer s.Close()
	if fr, err := drain(s); err != io.EOF || len(fr) != 2 {
		t.Fatalf("%v %d", err, len(fr))
	}
}

func TestStreamDefaultCapsCoverPython(t *testing.T) {
	if maxFrameBytes < 8<<20+1024 || maxStreamBytes < 9<<20-1 || maxStreamBytes > 16<<20 || maxStreamFrames < 20000 || maxStreamFrames > 25000 {
		t.Fatalf("caps %d %d %d", maxFrameBytes, maxStreamBytes, maxStreamFrames)
	}
}

func TestStreamCloseConcurrentWithBlockedNext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"t":"sse","seq":1,"d":1}` + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	s, _ := New(srv.URL, testToken, nil).Stream(context.Background(), "/internal/copilot/llm", struct{}{})
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, err := s.Next(); res <- err }()
	time.Sleep(100 * time.Millisecond)
	s.Close()
	wg.Wait()
	if err := <-res; !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}
