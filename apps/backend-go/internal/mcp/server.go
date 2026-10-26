package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

// Registry maps tool names to their handlers (built in Task 17).
type Registry struct {
	Reads  map[string]Tool
	Writes map[string]WriteTool
}

// NewRegistry indexes read and write tools by name.
func NewRegistry(reads []Tool, writes []WriteTool) Registry {
	r := Registry{Reads: map[string]Tool{}, Writes: map[string]WriteTool{}}
	for _, t := range reads {
		r.Reads[t.Name] = t
	}
	for _, t := range writes {
		r.Writes[t.Name] = t
	}
	return r
}

// ExecuteFunc runs one tools/call and returns its text payload. The write
// pipeline (execute.go) replaces the built-in default through Server.Execute.
type ExecuteFunc func(ctx context.Context, c Ctx, name string, args Args) string

// Server answers JSON-RPC messages for an authenticated Ctx.
type Server struct {
	d     Deps
	tools Registry
	// Execute runs tools/call; nil selects the built-in minimal executor
	// (unknown tool, capability and write-flag gates, read tools only).
	Execute ExecuteFunc
	// Timeout bounds one tool call; zero means Cfg.ToolTimeoutSeconds (120).
	Timeout time.Duration
}

// NewServer builds a Server over d and its tool registry.
func NewServer(d Deps, tools Registry) *Server { return &Server{d: d, tools: tools} }

func (s *Server) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	if n := s.d.Cfg.ToolTimeoutSeconds; n > 0 {
		return time.Duration(n) * time.Second
	}
	return 120 * time.Second
}

// List returns the public specs visible to c: the token holds the tool's
// capability and, for write capabilities, MCP_WRITE_ENABLED is on. The
// `capability` key is removed.
func (s *Server) List(c Ctx) []any {
	out := []any{}
	for _, t := range Specs {
		if !c.Has(t.Capability) {
			continue
		}
		if WriteCaps[t.Capability] && !s.d.Cfg.WriteEnabled {
			continue
		}
		out = append(out, pyjson.NewObj("name", t.Name, "description", t.Description, "inputSchema", t.InputSchema))
	}
	return out
}

// Handle answers one JSON-RPC message. respond is false for notifications.
// It never panics: malformed input gets a JSON-RPC error (D4).
func (s *Server) Handle(ctx context.Context, c Ctx, raw []byte) (resp []byte, respond bool) {
	id := idNull
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mcp handler panic", "panic", fmt.Sprint(r))
			resp, respond = internalErrorBytes(id), true
		}
	}()
	req, id, perr := parseRequest(raw)
	if perr != nil {
		return errorBytes(id, *perr), true
	}
	switch {
	case req.Method == "initialize":
		return resultBytes(id, pyjson.NewObj(
			"protocolVersion", protocolVersion,
			"capabilities", pyjson.NewObj("tools", pyjson.NewObj("listChanged", false)),
			"serverInfo", pyjson.NewObj("name", serverName, "version", serverVersion),
		)), true
	case strings.HasPrefix(req.Method, "notifications/"):
		return nil, false
	case req.Method == "tools/list":
		return resultBytes(id, pyjson.NewObj("tools", s.List(c))), true
	case req.Method == "tools/call":
		name := ""
		if v, ok := req.Params["name"]; ok {
			if name, ok = v.(string); !ok {
				return errorBytes(id, rpcError{codeInvalidParams, "Invalid params"}), true
			}
		}
		args := Args{}
		if v, ok := req.Params["arguments"]; ok && v != nil {
			m, isMap := v.(map[string]any)
			if !isMap {
				return errorBytes(id, rpcError{codeInvalidParams, "Invalid params"}), true
			}
			args = m
		}
		text := s.callTool(ctx, c, name, args)
		return resultBytes(id, pyjson.NewObj(
			"content", []any{pyjson.NewObj("type", "text", "text", text)},
			"isError", false,
		)), true
	case req.Method == "ping":
		return resultBytes(id, pyjson.NewObj()), true
	}
	return errorBytes(id, rpcError{codeMethodNotFound, "Method not found: " + req.Method}), true
}

// callTool runs the executor under the tool deadline. A handler that ignores
// its context is abandoned (the answer is not blocked on it).
func (s *Server) callTool(ctx context.Context, c Ctx, name string, args Args) string {
	exec := s.Execute
	if exec == nil {
		exec = s.defaultExecute
	}
	d := s.timeout()
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	done := make(chan string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("mcp tool panic", "tool", name, "panic", fmt.Sprint(r))
				done <- errorText("internal error")
			}
		}()
		done <- exec(cctx, c, name, args)
	}()
	select {
	case text := <-done:
		return text
	case <-cctx.Done():
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return errorText(fmt.Sprintf("tool timed out after %s", d))
		}
		return errorText("request cancelled")
	}
}

func errorText(msg string) string {
	b, err := pyjson.Dumps(pyjson.NewObj("error", msg), 0)
	if err != nil {
		return `{"error": "internal error"}`
	}
	return string(b)
}

// defaultExecute is the minimal executor used until the write pipeline is wired:
// Python's unknown-tool, capability and write-flag answers, and read tools.
func (s *Server) defaultExecute(ctx context.Context, c Ctx, name string, args Args) string {
	var capability string
	for _, t := range Specs {
		if t.Name == name {
			capability = t.Capability
		}
	}
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
		return errorText(ErrUnavailable.Error())
	}
	t, ok := s.tools.Reads[name]
	if !ok || t.Handler == nil {
		return errorText(ErrUnavailable.Error())
	}
	text, err := t.Handler(ctx, c, args)
	if err != nil {
		return errorText(safeErrorText(err))
	}
	return text
}

// safeErrorText renders only the error kinds the Tool contract allows to reach a
// client; everything else (driver and database text) is masked.
func safeErrorText(err error) string {
	var ae *ArgError
	var he *HTTPError
	switch {
	case errors.As(err, &ae):
		return ae.Error()
	case IsUnavailable(err):
		return ErrUnavailable.Error()
	case errors.As(err, &he):
		return he.Detail
	}
	return "internal error"
}
