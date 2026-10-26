package leadevents

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestStreamHeartbeatAfterSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var buf syncBuf
	done := make(chan error, 1)
	go func() { done <- NewRelay(nil, 20*time.Millisecond).Stream(ctx, "ws", &buf, func() {}) }()
	time.Sleep(110 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), string(HeartbeatFrame())); n < 2 {
		t.Fatalf("heartbeats = %d (%q)", n, buf.String())
	}
}
