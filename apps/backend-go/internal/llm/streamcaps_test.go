package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamFrames runs one Stream against handler and returns its frames (failing when it does not end in time).
func streamFrames(t *testing.T, timeout time.Duration, handler http.HandlerFunc) []Frame {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	c := newTestClient(t, mapSettings{}, nil, nil)
	if timeout > 0 {
		c.o.StreamTimeout = timeout
	}
	var out []Frame
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range c.Stream(context.Background(), StreamRequest{Cursor: Cursor{Provider: ChatProvider{ID: "p", APIKey: "k", BaseURL: srv.URL}}}) {
			out = append(out, f)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end")
	}
	return out
}

func hasLimitError(fs []Frame) bool {
	for _, f := range fs {
		if f.Kind == FrameError && strings.Contains(f.Text, "limit exceeded") {
			return true
		}
	}
	return false
}

func TestStreamCapsLines(t *testing.T) {
	old := maxStreamLines
	maxStreamLines = 50
	defer func() { maxStreamLines = old }()
	fs := streamFrames(t, 0, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 5000 && r.Context().Err() == nil; i++ {
			fmt.Fprint(w, ": ka\n") // comments are lines too
			w.(http.Flusher).Flush()
		}
	})
	if !hasLimitError(fs) {
		t.Fatalf("line cap not enforced: %+v", fs)
	}
}

func TestStreamCapsToolCallArgumentBytes(t *testing.T) {
	old := maxStreamBytes
	maxStreamBytes = 4 << 10
	defer func() { maxStreamBytes = old }()
	fs := streamFrames(t, 0, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 100000 && r.Context().Err() == nil; i++ {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":"x"}}]}}]}`+"\n\n")
			w.(http.Flusher).Flush()
		}
	})
	if !hasLimitError(fs) {
		t.Fatalf("argument byte cap not enforced: %d frames", len(fs))
	}
	for _, f := range fs {
		if f.Kind == FrameToolCalls {
			t.Fatal("an over-cap tool call was delivered")
		}
	}
}

func TestStreamCapsRawBytes(t *testing.T) {
	old := maxStreamBytes
	maxStreamBytes = 4 << 10
	defer func() { maxStreamBytes = old }()
	fs := streamFrames(t, 0, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 100000 && r.Context().Err() == nil; i++ {
			fmt.Fprint(w, ": "+strings.Repeat("k", 200)+"\n")
			w.(http.Flusher).Flush()
		}
	})
	if !hasLimitError(fs) {
		t.Fatal("raw byte cap not enforced")
	}
}

func TestStreamCapsWallClock(t *testing.T) {
	old := maxStreamDuration
	maxStreamDuration = 300 * time.Millisecond
	defer func() { maxStreamDuration = old }()
	fs := streamFrames(t, 0, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 500 && r.Context().Err() == nil; i++ {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"."}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	if !hasLimitError(fs) {
		t.Fatalf("wall clock not enforced: %d frames", len(fs))
	}
}

// Keep-alive comments are not progress: a stalled provider times out even if it keeps the socket busy.
func TestStreamIdleIgnoresKeepAlives(t *testing.T) {
	fs := streamFrames(t, 300*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 500 && r.Context().Err() == nil; i++ {
			fmt.Fprint(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	var timed bool
	for _, f := range fs {
		if f.Kind == FrameError && strings.Contains(f.Text, "timed out") {
			timed = true
		}
	}
	if !timed {
		t.Fatalf("keep-alives kept the stream alive: %+v", fs)
	}
}
