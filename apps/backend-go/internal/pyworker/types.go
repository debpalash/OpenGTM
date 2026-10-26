// Package pyworker is the boundary to the Python worker's POST
// /internal/provider-call RPC (decision D8). This file holds the wire types.
package pyworker

import (
	"encoding/json"
	"time"
)

// ProviderCall is one provider invocation delegated to Python. Credentials are
// resolved on the Python side from WorkspaceID; they never cross the wire.
type ProviderCall struct {
	CallID      string
	Provider    string
	WorkspaceID string
	Input       json.RawMessage
	Timeout     time.Duration
}

// ProviderResult is the Python answer. Status is "ok", "error" or "timeout".
type ProviderResult struct {
	Status  string
	Output  json.RawMessage
	Error   string
	CostUSD float64
}
