package mcp

import (
	"context"
	"encoding/json"
)

type rawArgsKey struct{}

// withRawArgs carries the tools/call arguments as sent. Args is a Go map (key
// order lost); a tool that stores client JSON (create_automation's
// trigger_config and actions) reads the original bytes so the stored rule keeps
// the client's key order, as Python's dicts did.
func withRawArgs(ctx context.Context, raw json.RawMessage) context.Context {
	if len(raw) == 0 {
		return ctx
	}
	return context.WithValue(ctx, rawArgsKey{}, raw)
}

// orderedArg returns the raw JSON of argument key as sent, or nil when the
// call did not come through Handle or the key is absent.
func orderedArg(ctx context.Context, key string) json.RawMessage {
	raw, _ := ctx.Value(rawArgsKey{}).(json.RawMessage)
	if raw == nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m[key]
}
