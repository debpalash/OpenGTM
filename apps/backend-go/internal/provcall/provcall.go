// Package provcall is the provider-call shape shared by the in-process provider runners (rpclocal,
// providerrun) and their callers (email verification, person intel, collect): one named provider invoked with
// JSON input for a workspace. Credentials are resolved by the runner from WorkspaceID; they are never part of
// the call. It replaces the wire types of the retired Python provider RPC (plan 15).
package provcall

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Call is one provider invocation.
type Call struct {
	CallID      string
	Provider    string
	WorkspaceID string
	Input       json.RawMessage
	Timeout     time.Duration
}

// Result is a provider answer. Status is "ok", "error" or "timeout".
type Result struct {
	Status  string
	Output  json.RawMessage
	Error   string
	CostUSD float64
	// HTTPStatus is the HTTP status of a rejected or failed call (0 when none applies). Err is a sentinel
	// (ErrUnknownProvider, ErrUnauthorized) for rejections callers may act on; nil otherwise.
	HTTPStatus int
	Err        error
}

// ErrUnavailable means no runner serves the provider in this process.
var ErrUnavailable = errors.New("provcall: provider unavailable")

// ErrUnknownProvider is Result.Err for a provider no runner knows.
var ErrUnknownProvider = errors.New("provcall: unknown provider")

// ErrUnauthorized is Result.Err for a configuration fault of the runner (never retried).
var ErrUnauthorized = errors.New("provcall: provider runner rejected the call")

// Caller runs provider calls (rpclocal.Caller).
type Caller interface {
	Call(ctx context.Context, call Call) (Result, error)
}
