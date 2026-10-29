package copilot

import (
	"context"
	"encoding/json"
)

// Frame is one frame of an llm round or a pre-route branch. T is the frame type (content, warning, error,
// tool_calls, done; pre-route: sse, persist, none, done, error). Unknown types are ignored by the consumers.
type Frame struct {
	T        string
	Seq      int
	D        json.RawMessage
	Role     *string
	Content  *string
	ToolData *string
	Cursor   *int
	Message  string
	// ProviderDone is non-nil only when the frame carries it. A done frame with ProviderDone == false means
	// the provider ended without [DONE]: the turn must NOT send [DONE].
	ProviderDone *bool
}

// FrameSource is one llm round's frames. Next is single-goroutine; Close may be called from any goroutine.
type FrameSource interface {
	Next() (Frame, error)
	Close()
}

// RoundFunc runs one llm round (Deps.Rounds).
type RoundFunc func(ctx context.Context, rq RoundRequest) (FrameSource, error)
