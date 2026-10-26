package mcp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

// toolCapability returns the capability of a catalog tool ("" = unknown tool).
func toolCapability(name string) string {
	for _, t := range Specs {
		if t.Name == name {
			return t.Capability
		}
	}
	return ""
}

// execute is execute_tool: unknown tool, capability and write-flag gates (write
// denials are audited), then the read handler or the guarded write pipeline.
func (s *Server) execute(ctx context.Context, c Ctx, name string, args Args) string {
	capability := toolCapability(name)
	if capability == "" {
		return errorText("Unknown tool: " + name)
	}
	isWrite := WriteCaps[capability]
	if !c.Has(capability) {
		if isWrite {
			Record(ctx, s.d, c, name, args, AuditOutcome{Status: "denied", Error: "capability not granted"})
		}
		b, _ := pyjson.Dumps(pyjson.NewObj("error", "capability not granted", "required", capability), 0)
		return string(b)
	}
	if isWrite {
		if !s.d.Cfg.WriteEnabled {
			Record(ctx, s.d, c, name, args, AuditOutcome{Status: "denied", Error: "mcp writes disabled"})
			return errorText("mcp writes disabled")
		}
		return s.executeWrite(ctx, c, name, args)
	}
	t, ok := s.tools.Reads[name]
	if !ok || t.Handler == nil {
		return errorText(ErrUnavailable.Error())
	}
	return s.runRead(ctx, c, t, args)
}

// runRead runs a read handler; an error or panic becomes {"error": ...} with
// only client-safe text (see Tool).
func (s *Server) runRead(ctx context.Context, c Ctx, t Tool, args Args) (text string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mcp read tool panic", "tool", t.Name, "panic", fmt.Sprint(r))
			text = errorText("internal error")
		}
	}()
	out, err := t.Handler(ctx, c, args)
	if err != nil {
		return errorText(safeErrorText(err))
	}
	return out
}
