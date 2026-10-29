package mcp

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

// MaxMessageBytes bounds one JSON-RPC message (D4). Transports apply their own,
// usually tighter, frame limits before the bytes reach Server.Handle.
const MaxMessageBytes = 4 << 20

// JSON-RPC protocol constants (apps/mcp/server.py).
const (
	jsonRPCVersion  = "2.0"
	protocolVersion = "2024-11-05"
	serverName      = "opengtm"
	serverVersion   = "3.0.0"

	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// rpcError is a JSON-RPC error answer: the code and message are wire parity.
type rpcError struct {
	Code    int64
	Message string
}

// request is a validated JSON-RPC message. ID is the already-rendered JSON text
// of the request id as Python would echo it (number, string or null).
type request struct {
	ID        string
	Method    string
	Params    map[string]any // nil when params is absent
	HasParams bool
	// RawArgs is params.arguments as sent (nil when absent): Params loses object
	// key order, which a stored automation rule keeps (orderedArg).
	RawArgs json.RawMessage
}

// idNull is the id of an answer to a request whose id could not be read.
const idNull = "null"

// parseRequest validates raw. On failure the rpcError is the answer and id is
// the text to echo (idNull when the id is unreadable).
func parseRequest(raw []byte) (req request, id string, perr *rpcError) {
	id = idNull
	if len(raw) > MaxMessageBytes {
		return req, id, &rpcError{codeInvalidRequest, "Invalid Request"}
	}
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return req, id, &rpcError{codeParseError, "Parse error"}
	}
	if tooDeep(raw) {
		return req, id, &rpcError{codeInvalidRequest, "Invalid Request"}
	}
	if t := bytes.TrimLeft(raw, " \t\r\n"); len(t) == 0 || t[0] != '{' {
		return req, id, &rpcError{codeInvalidRequest, "Invalid Request"}
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return req, id, &rpcError{codeInvalidRequest, "Invalid Request"}
	}
	if rawID, ok := env["id"]; ok {
		text, ok := renderID(rawID)
		if !ok {
			return req, id, &rpcError{codeInvalidRequest, "Invalid Request"}
		}
		id = text
	}
	req.ID = id
	if rawMethod, ok := env["method"]; ok {
		if err := json.Unmarshal(rawMethod, &req.Method); err != nil || string(bytes.TrimSpace(rawMethod)) == "null" {
			return req, id, &rpcError{codeInvalidRequest, "Invalid Request"}
		}
	}
	if rawParams, ok := env["params"]; ok {
		req.HasParams = true
		// Notifications carry no answer, so their params are not inspected.
		if !strings.HasPrefix(req.Method, "notifications/") {
			v, err := canonjson.Decode(rawParams)
			m, isMap := v.(map[string]any)
			if err != nil || !isMap {
				return req, id, &rpcError{codeInvalidParams, "Invalid params"}
			}
			req.Params = m
			var raw map[string]json.RawMessage
			if json.Unmarshal(rawParams, &raw) == nil {
				req.RawArgs = raw["arguments"]
			}
		}
	}
	return req, id, nil
}

// tooDeep reports container nesting deeper than canonjson.MaxDepth.
func tooDeep(raw []byte) bool {
	depth, inStr, esc := 0, false, false
	for _, b := range raw {
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case b == '\\':
				esc = true
			case b == '"':
				inStr = false
			}
		case b == '"':
			inStr = true
		case b == '{' || b == '[':
			depth++
			if depth > canonjson.MaxDepth {
				return true
			}
		case b == '}' || b == ']':
			depth--
		}
	}
	return false
}

// renderID renders a JSON-RPC id as Python's json.dumps would echo it after a
// json.loads round trip: integers keep their digits ("-0" is 0), floats use
// repr, strings are escaped, null stays null. Objects, arrays and booleans are
// not valid ids.
func renderID(raw json.RawMessage) (string, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return "", false
	}
	switch c := t[0]; {
	case string(t) == "null":
		return idNull, true
	case c == '"':
		var s string
		if json.Unmarshal(t, &s) != nil {
			return "", false
		}
		out, err := pyjson.Dumps(s, 0)
		if err != nil {
			return "", false
		}
		return string(out), true
	case c == '-' || (c >= '0' && c <= '9'):
		s := string(t)
		if strings.ContainsAny(s, ".eE") {
			f, err := strconv.ParseFloat(s, 64) // +-Inf on overflow, as Python
			if f == 0 && err != nil {
				return "", false
			}
			r, err := canonjson.FloatRepr(f)
			if err != nil {
				return "", false
			}
			return r, true
		}
		if s == "-0" {
			return "0", true
		}
		return s, true
	}
	return "", false
}

// envelope renders {"jsonrpc": "2.0", "id": <id>, <key>: <body>} with Python
// default separators.
func envelope(id, key string, body any) []byte {
	b, err := pyjson.Dumps(body, 0)
	if err != nil {
		return internalErrorBytes(id)
	}
	return []byte(`{"jsonrpc": "2.0", "id": ` + id + `, "` + key + `": ` + string(b) + `}`)
}

func resultBytes(id string, result *pyjson.Obj) []byte { return envelope(id, "result", result) }

func errorBytes(id string, e rpcError) []byte {
	return envelope(id, "error", pyjson.NewObj("code", e.Code, "message", e.Message))
}

func internalErrorBytes(id string) []byte {
	return []byte(`{"jsonrpc": "2.0", "id": ` + id + `, "error": {"code": -32603, "message": "Internal error"}}`)
}
