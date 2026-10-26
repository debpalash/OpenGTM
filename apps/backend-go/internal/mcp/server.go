package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
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

// BuildRegistry concatenates the six tool constructors and refuses to start
// (panics, recovered by cmd/mcp into exit 1) unless the registered names and
// capabilities equal the 15-tool catalog.
func BuildRegistry(d Deps) Registry {
	reads := append(readTools(d), enrichTools(d)...)
	var writes []WriteTool
	for _, ws := range [][]WriteTool{leadWriteTools(d), workbookWriteTools(d), outreachWriteTools(d), automationWriteTools(d)} {
		writes = append(writes, ws...)
	}
	r := NewRegistry(reads, writes)
	if err := CheckRegistry(r); err != nil {
		panic("mcp: " + err.Error())
	}
	return r
}

// CheckRegistry verifies r holds exactly the catalog tools, each with the
// catalog capability and on the matching side (read vs write capability).
func CheckRegistry(r Registry) error {
	var problems []string
	seen := map[string]bool{}
	for _, s := range Specs {
		seen[s.Name] = true
		var capability string
		_, isRead := r.Reads[s.Name]
		_, isWrite := r.Writes[s.Name]
		switch {
		case isRead && isWrite:
			problems = append(problems, s.Name+" registered as read and write")
			continue
		case isRead:
			capability = r.Reads[s.Name].Capability
		case isWrite:
			capability = r.Writes[s.Name].Capability
		default:
			problems = append(problems, "missing tool "+s.Name)
			continue
		}
		if capability != s.Capability {
			problems = append(problems, fmt.Sprintf("%s has capability %q, catalog says %q", s.Name, capability, s.Capability))
		}
		if isWrite != WriteCaps[s.Capability] {
			problems = append(problems, s.Name+" is on the wrong side (read/write) for its capability")
		}
	}
	var extra []string
	for n := range r.Reads {
		if !seen[n] {
			extra = append(extra, n)
		}
	}
	for n := range r.Writes {
		if !seen[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	for _, n := range extra {
		problems = append(problems, "unexpected tool "+n)
	}
	if len(problems) > 0 {
		return fmt.Errorf("tool registry does not match the catalog: %s", strings.Join(problems, "; "))
	}
	return nil
}

// ExecuteFunc runs one tools/call and returns its text payload. The write
// pipeline (execute.go) replaces the built-in default through Server.Execute.
type ExecuteFunc func(ctx context.Context, c Ctx, name string, args Args) string

// Server answers JSON-RPC messages for an authenticated Ctx.
type Server struct {
	d     Deps
	tools Registry
	// Execute runs tools/call; NewServer sets it to the execute flow (execute.go).
	Execute ExecuteFunc
	// Timeout bounds one tool call; zero means Cfg.ToolTimeoutSeconds (120).
	Timeout time.Duration
	// MaxInFlightWrites bounds concurrent write calls per workspace (including
	// calls abandoned by a timeout that are still unwinding); zero means 8.
	MaxInFlightWrites int

	inflightMu sync.Mutex
	inflight   map[string]int
}

// NewServer builds a Server over d and its tool registry.
func NewServer(d Deps, tools Registry) *Server {
	s := &Server{d: d, tools: tools, inflight: map[string]int{}}
	s.Execute = s.execute
	return s
}

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
		exec = s.execute
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
