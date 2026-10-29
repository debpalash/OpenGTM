package copilot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

func capSrc(in <-chan llm.Frame) *nativeSrc {
	ctx, cancel := context.WithCancel(context.Background())
	return &nativeSrc{ctx: ctx, cancel: cancel, in: in, chain: []llm.ChatProvider{{ID: "p"}}, started: time.Now()}
}

// drainErr reads frames until the round ends and returns the error text, if any.
func drainErr(t *testing.T, s *nativeSrc) string {
	t.Helper()
	defer s.Close()
	done := make(chan string, 1)
	go func() {
		for {
			f, err := s.Next()
			if err != nil {
				done <- ""
				return
			}
			if f.T == "error" {
				done <- string(f.D)
				return
			}
		}
	}()
	select {
	case e := <-done:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("round did not end")
		return ""
	}
}

func feed(frames func(send func(llm.Frame) bool)) <-chan llm.Frame {
	ch := make(chan llm.Frame)
	go func() {
		defer close(ch)
		frames(func(f llm.Frame) bool {
			select {
			case ch <- f:
				return true
			case <-time.After(5 * time.Second):
				return false
			}
		})
	}()
	return ch
}

func TestRoundCapsFrames(t *testing.T) {
	old := maxRoundFrames
	maxRoundFrames = 10
	defer func() { maxRoundFrames = old }()
	in := feed(func(send func(llm.Frame) bool) {
		for i := 0; i < 100 && send(llm.Frame{Kind: llm.FrameContent, Text: "x"}); i++ {
		}
	})
	if e := drainErr(t, capSrc(in)); !strings.Contains(e, "stream limit exceeded") {
		t.Fatalf("frame cap not enforced: %q", e)
	}
}

// A tool_calls frame weighs its arguments (previously a flat 64 bytes per call).
func TestRoundCapsToolCallArgumentBytes(t *testing.T) {
	old := maxRoundBytes
	maxRoundBytes = 1 << 10
	defer func() { maxRoundBytes = old }()
	in := feed(func(send func(llm.Frame) bool) {
		send(llm.Frame{Kind: llm.FrameToolCalls, ToolCalls: []llm.ToolCall{{ID: "c", Function: llm.ToolFunction{Name: "n", Arguments: strings.Repeat("a", 4<<10)}}}})
	})
	if e := drainErr(t, capSrc(in)); !strings.Contains(e, "stream limit exceeded") {
		t.Fatalf("argument bytes not counted: %q", e)
	}
}

func TestRoundCapsContentBytes(t *testing.T) {
	old := maxRoundBytes
	maxRoundBytes = 1 << 10
	defer func() { maxRoundBytes = old }()
	in := feed(func(send func(llm.Frame) bool) {
		for i := 0; i < 100 && send(llm.Frame{Kind: llm.FrameContent, Text: strings.Repeat("a", 100)}); i++ {
		}
	})
	if e := drainErr(t, capSrc(in)); !strings.Contains(e, "stream limit exceeded") {
		t.Fatalf("byte cap not enforced: %q", e)
	}
}

// A provider that never produces a frame (or trickles) cannot hold the round past the wall clock: the check no
// longer waits for a frame to arrive.
func TestRoundWallClockFiresWithoutFrames(t *testing.T) {
	old := maxRoundSeconds
	maxRoundSeconds = 200 * time.Millisecond
	defer func() { maxRoundSeconds = old }()
	block := make(chan llm.Frame) // never delivers, never closes
	start := time.Now()
	if e := drainErr(t, capSrc(block)); !strings.Contains(e, "stream limit exceeded") {
		t.Fatalf("wall clock not enforced: %q", e)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("took too long")
	}
}
