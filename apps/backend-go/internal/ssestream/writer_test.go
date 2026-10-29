package ssestream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newW(t *testing.T) (*Writer, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	w, err := New(rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w, rec
}

// Vectors are Python json.dumps(v) (default separators, ensure_ascii, insertion order).
func TestEventBytesMatchPythonDumps(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"simple", Obj{{"conversation_id", "abc"}}, `{"conversation_id": "abc"}`},
		{"non-ascii", Obj{{"content", " from the simulator \u2014 caf\u00e9 \u2713 \U0001F600"}}, `{"content": " from the simulator \u2014 caf\u00e9 \u2713 \ud83d\ude00"}`},
		{"insertion order", Obj{{"confirmation_id", "c1"}, {"tool_call", "t1"}, {"name", "n"}, {"args", Obj{{"z", int64(1)}, {"a", []any{}}}}, {"level", nil}}, `{"confirmation_id": "c1", "tool_call": "t1", "name": "n", "args": {"z": 1, "a": []}, "level": null}`},
		{"floats", Obj{{"a", 1.0}, {"b", 0.1}, {"c", 1e16}, {"d", 1.5e-7}, {"e", int64(-3)}, {"f", true}}, `{"a": 1.0, "b": 0.1, "c": 1e+16, "d": 1.5e-07, "e": -3, "f": true}`},
		{"map sorted", map[string]any{"b": "x", "a": []any{int64(1), "y"}}, `{"a": [1, "y"], "b": "x"}`},
		{"empty obj", Obj{}, `{}`},
		{"escapes", Obj{{"s", "a\"b\\c\n\t\u0001"}}, `{"s": "a\"b\\c\n\t\u0001"}`},
		{"int", Obj{{"n", 7}, {"m", int32(8)}}, `{"n": 7, "m": 8}`},
	}
	for _, c := range cases {
		w, rec := newW(t)
		if err := w.Event(c.v); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got, want := rec.Body.String(), "data: "+c.want+"\n\n"; got != want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, want)
		}
		if !rec.Flushed {
			t.Errorf("%s: not flushed", c.name)
		}
	}
}

func TestEventRejectsUnsupported(t *testing.T) {
	w, rec := newW(t)
	if err := w.Event(make(chan int)); err == nil {
		t.Fatal("want error")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("partial frame written: %q", rec.Body.String())
	}
}

func TestDoneAndKeepaliveBytes(t *testing.T) {
	w, rec := newW(t)
	if err := w.Keepalive(); err != nil {
		t.Fatal(err)
	}
	if err := w.Done(); err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != ": keepalive\n\ndata: [DONE]\n\n" {
		t.Fatalf("got %q", got)
	}
}

func TestHeadersMatchFixture(t *testing.T) {
	rec := httptest.NewRecorder()
	Headers(rec)
	want := map[string]string{
		"Content-Type":      "text/event-stream; charset=utf-8", // as recorded from FastAPI
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	if len(rec.Header()) != len(want) {
		t.Fatalf("headers %v", rec.Header())
	}
	for k, v := range want {
		if rec.Header().Get(k) != v {
			t.Errorf("%s = %q want %q", k, rec.Header().Get(k), v)
		}
	}
}

type noFlush struct{ http.ResponseWriter }

func TestNewRequiresFlusher(t *testing.T) {
	if _, err := New(noFlush{httptest.NewRecorder()}, nil); err == nil {
		t.Fatal("want error")
	}
}

type failingRW struct {
	*httptest.ResponseRecorder
	fail bool
}

func (f *failingRW) Write(b []byte) (int, error) {
	if f.fail {
		return 0, errors.New("broken pipe")
	}
	return f.ResponseRecorder.Write(b)
}

func TestWriterErrorAfterClientGone(t *testing.T) {
	rw := &failingRW{ResponseRecorder: httptest.NewRecorder()}
	w, err := New(rw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Event(Obj{{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	rw.fail = true
	if err := w.Event(Obj{{"a", "c"}}); err == nil {
		t.Fatal("want write error")
	}
	rw.fail = false // sticky: nothing is written after the first failure
	if err := w.Keepalive(); err == nil {
		t.Fatal("want sticky error")
	}
	if err := w.Done(); err == nil {
		t.Fatal("want sticky error")
	}
	if got := rw.Body.String(); got != "data: {\"a\": \"b\"}\n\n" {
		t.Fatalf("got %q", got)
	}
}

func TestCustomFlushCalled(t *testing.T) {
	n := 0
	w, err := New(httptest.NewRecorder(), func() { n++ })
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Event(Obj{})
	_ = w.Done()
	if n != 2 {
		t.Fatalf("flush calls %d", n)
	}
}

func TestKeepaliveLoopEmitsAndStopsWhenDone(t *testing.T) {
	w, rec := newW(t)
	done := make(chan struct{})
	fin := make(chan struct{})
	go func() { KeepaliveLoop(context.Background(), 10*time.Millisecond, w, done); close(fin) }()
	time.Sleep(55 * time.Millisecond)
	close(done)
	select {
	case <-fin:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop")
	}
	n := strings.Count(rec.Body.String(), ": keepalive\n\n")
	if n < 2 {
		t.Fatalf("keepalives %d", n)
	}
	if strings.Replace(rec.Body.String(), ": keepalive\n\n", "", -1) != "" {
		t.Fatalf("unexpected bytes %q", rec.Body.String())
	}
}

func TestKeepaliveStopsWhenDone(t *testing.T) {
	TestKeepaliveLoopEmitsAndStopsWhenDone(t)
}

func TestKeepaliveStopsOnCtxAndWriteError(t *testing.T) {
	w, _ := newW(t)
	ctx, cancel := context.WithCancel(context.Background())
	fin := make(chan struct{})
	go func() { KeepaliveLoop(ctx, time.Hour, w, make(chan struct{})); close(fin) }()
	cancel()
	select {
	case <-fin:
	case <-time.After(time.Second):
		t.Fatal("ctx did not stop loop")
	}
	rw := &failingRW{ResponseRecorder: httptest.NewRecorder(), fail: true}
	w2, _ := New(rw, nil)
	fin2 := make(chan struct{})
	go func() { KeepaliveLoop(context.Background(), 5*time.Millisecond, w2, make(chan struct{})); close(fin2) }()
	select {
	case <-fin2:
	case <-time.After(time.Second):
		t.Fatal("write error did not stop loop")
	}
}

func TestConcurrentKeepaliveAndEventNoInterleave(t *testing.T) {
	w, rec := newW(t)
	fin := make(chan struct{})
	done := make(chan struct{})
	go func() { KeepaliveLoop(context.Background(), time.Millisecond, w, done); close(fin) }()
	for i := 0; i < 200; i++ {
		_ = w.Event(Obj{{"i", int64(i)}})
	}
	close(done)
	<-fin
	for _, f := range strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n") {
		if f != ": keepalive" && !strings.HasPrefix(f, `data: {"i": `) {
			t.Fatalf("corrupt frame %q", f)
		}
	}
}
