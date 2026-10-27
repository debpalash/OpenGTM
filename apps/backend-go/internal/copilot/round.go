package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// jstr is a JSON string literal with Python's ensure_ascii escapes. Invalid UTF-8 is replaced first.
func jstr(s string) string {
	b, err := canonjson.Marshal(strings.ToValidUTF8(s, "�"), false)
	if err != nil {
		return `""`
	}
	return string(b)
}

type roundRequest struct {
	messages     []map[string]any
	cursor       int
	toolsEnabled bool
	budgetHit    bool
}

type roundResult struct {
	hasCalls bool
	calls    json.RawMessage
	cursor   int
}

// round performs one llm RPC: content, warning and error frames are relayed as they arrive; a tool_calls
// frame ends the round with the calls and the provider cursor. A stream that ends without a terminal
// frame, or any protocol fault, becomes a turnErr (one fixed error frame and [DONE]).
func (t *Turn) round(ctx context.Context, w *ssestream.Writer, rq roundRequest) (roundResult, error) {
	st, err := t.openRound(ctx, rq)
	if err != nil {
		return roundResult{}, t.streamErr(ctx, err)
	}
	defer st.Close()
	for {
		f, err := st.Next()
		if err != nil {
			return roundResult{}, t.streamErr(ctx, err)
		}
		switch f.T {
		case "content", "warning", "error":
			d := []byte(f.D)
			if len(d) == 0 {
				d = []byte("null")
			}
			if err := t.emitKey(w, f.T, d); err != nil {
				return roundResult{}, t.relayErr(ctx, err)
			}
			if f.T == "error" {
				return roundResult{}, nil
			}
		case "done":
			if f.ProviderDone == nil || *f.ProviderDone {
				if err := w.Done(); err != nil {
					return roundResult{}, err
				}
			}
			return roundResult{}, nil
		case "tool_calls":
			c := rq.cursor
			if f.Cursor != nil {
				c = *f.Cursor
			}
			return roundResult{hasCalls: true, calls: f.D, cursor: c}, nil
		}
	}
}

// streamErr classifies a pyworker failure: a cancelled context is not a provider fault.
func (t *Turn) streamErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &turnErr{errTextInterrupted, err}
}

// relayErr is a failure to write a relayed worker frame: a frame that is not valid JSON is a protocol
// fault; anything else is the client's connection.
func (t *Turn) relayErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errBadFrame) {
		return &turnErr{errTextInterrupted, err}
	}
	return err
}

// ---- tool calls ----

// parsedCall is one provider tool call after Python's argument handling.
type parsedCall struct {
	ID, Name  string
	ArgsStr   string         // the arguments string as the provider sent it
	ArgsJSON  []byte         // arguments rendered like json.dumps, "{}" when not a JSON object
	Args      map[string]any // decoded arguments, empty when not a JSON object
	Dangerous bool           // everything not explicitly read-only: unknown names fail closed
	tc        map[string]any // the call object with function.arguments "" fixed to "{}"
	raw       []byte         // the call object as the worker sent it
}

// fnArguments is the arguments string the provider sees on the next round.
func (c parsedCall) fnArguments() string {
	if c.ArgsStr == "" {
		return "{}"
	}
	return c.ArgsStr
}

func (c parsedCall) dedupKey() string {
	b, err := canonjson.Marshal(c.Args, true)
	if err != nil {
		b = []byte(c.ArgsStr)
	}
	return c.Name + "|" + string(b)
}

func callsAny(cs []parsedCall) []any {
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = c.tc
	}
	return out
}

// parseToolCalls decodes a tool_calls frame payload.
func parseToolCalls(raw json.RawMessage) ([]parsedCall, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, errors.New("copilot: tool_calls frame is not a list")
	}
	out := make([]parsedCall, 0, len(items))
	for _, it := range items {
		v, err := canonjson.Decode(it)
		if err != nil {
			return nil, errors.New("copilot: tool call is not valid JSON")
		}
		tc, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("copilot: tool call is not an object")
		}
		fn, ok := tc["function"].(map[string]any)
		if !ok {
			return nil, errors.New("copilot: tool call has no function")
		}
		name, ok := fn["name"].(string)
		if !ok || name == "" {
			return nil, errors.New("copilot: tool call has no name")
		}
		var argStr string
		switch a := fn["arguments"].(type) {
		case nil:
		case string:
			argStr = a
		default:
			return nil, errors.New("copilot: tool call arguments are not a string")
		}
		id, _ := tc["id"].(string)
		if argStr == "" {
			fn["arguments"] = "{}"
		}
		c := parsedCall{ID: id, Name: name, ArgsStr: argStr, Dangerous: NeedsConfirmation(name), tc: tc, raw: it,
			ArgsJSON: []byte("{}"), Args: map[string]any{}}
		src := argStr
		if src == "" {
			src = "{}"
		}
		if av, err := canonjson.Decode([]byte(src)); err == nil {
			if m, ok := av.(map[string]any); ok {
				if aj, err := ssestream.Reformat([]byte(src)); err == nil {
					c.Args, c.ArgsJSON = m, aj
				}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// toolResultValue is the JSON of a tool_result frame's result: the parsed tool string, or {"raw": ...}
// when the tool did not answer JSON (Python would raise there).
func toolResultValue(s string) []byte {
	s = strings.ToValidUTF8(s, "�")
	if r, err := ssestream.Reformat([]byte(s)); err == nil {
		return r
	}
	return []byte(`{"raw": ` + jstr(s) + `}`)
}

// runSafe executes the safe calls in order, emitting tool_call then tool_result for each (dangerous calls
// are skipped here: they are gated, never executed) and returns the tool messages for the next round.
func (t *Turn) runSafe(ctx context.Context, w *ssestream.Writer, calls []parsedCall) ([]map[string]any, error) {
	var msgs []map[string]any
	for _, c := range calls {
		if c.Dangerous {
			continue
		}
		if err := t.emit(w, []byte(`{"tool_call": {"name": `+jstr(c.Name)+`, "args": `+string(c.ArgsJSON)+`}}`)); err != nil {
			return nil, err
		}
		key := c.dedupKey()
		result, seen := t.Seen[key]
		if !seen {
			var err error
			if result, err = t.await(ctx, w, c); err != nil {
				return nil, err
			}
			t.Seen[key] = result
		}
		result = t.budgetResult(result)
		if err := t.emit(w, []byte(`{"tool_result": {"name": `+jstr(c.Name)+`, "result": `+string(toolResultValue(result))+`}}`)); err != nil {
			return nil, err
		}
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.ID, "name": c.Name, "content": result})
	}
	return msgs, nil
}

// budgetResult charges result against the turn's aggregate tool-result budget; once it is spent the model gets a
// short marker instead (a cached duplicate costs again, since it goes into the prompt again).
func (t *Turn) budgetResult(result string) string {
	if t.toolBytes+len(result) > maxTurnToolBytes {
		return `{"error": "tool results for this turn exceed the size limit; answer from what you already have"}`
	}
	t.toolBytes += len(result)
	return result
}

type toolOutcome struct {
	result string
	err    error
}

// await runs the tool in a goroutine while keepalive comments flow every interval.
func (t *Turn) await(ctx context.Context, w *ssestream.Writer, c parsedCall) (string, error) {
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan toolOutcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- toolOutcome{result: toolErr(c.Name, "internal error")}
			}
		}()
		res, err := t.callTool(tctx, c)
		ch <- toolOutcome{res, err}
	}()
	tick, stop := t.ticker()
	defer stop()
	for {
		select {
		case o := <-ch:
			return o.result, o.err
		case <-tick:
			if err := w.Keepalive(); err != nil {
				cancel()
				<-ch
				return "", err
			}
		case <-ctx.Done():
			cancel()
			<-ch
			return "", ctx.Err()
		}
	}
}

func toolErr(name, why string) string {
	return `{"error": ` + jstr("Tool '"+name+"' failed: "+why) + `}`
}

func isNativeTool(name string) bool {
	for _, n := range nativeTools {
		if n == name {
			return true
		}
	}
	return false
}

// callTool runs one safe tool. A non-nil error is fatal for the turn (cancelled context, no tool user);
// every tool-side failure is a model-visible {"error": ...} result instead.
func (t *Turn) callTool(ctx context.Context, c parsedCall) (string, error) {
	res, err := t.callToolRaw(ctx, c)
	if err != nil {
		return "", err
	}
	if len(res) > maxToolResultBytes {
		return `{"error": "tool result too large"}`, nil
	}
	return res, nil
}

func (t *Turn) callToolRaw(ctx context.Context, c parsedCall) (string, error) {
	if isNativeTool(c.Name) {
		nctx, cancel := context.WithTimeout(ctx, t.toolTimeout())
		res, handled, err := t.native(nctx, c)
		cancel()
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return toolErr(c.Name, "timed out"), nil
		case err != nil:
			t.logger().Warn("copilot native tool failed", "tool", c.Name)
			return toolErr(c.Name, "internal error"), nil
		case handled:
			return res, nil
		}
	}
	if t.Engine != nil && isEngineTool(c.Name) {
		nctx, cancel := context.WithTimeout(ctx, t.toolTimeout())
		res, handled, err := t.engineTool(nctx, c)
		cancel()
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return toolErr(c.Name, "timed out"), nil
		case err != nil:
			t.logger().Warn("copilot engine tool failed", "tool", c.Name)
			return toolErr(c.Name, "internal error"), nil
		case handled:
			return res, nil
		}
	}
	return t.pythonTool(ctx, c)
}

func (t *Turn) native(ctx context.Context, c parsedCall) (string, bool, error) {
	if t.Native != nil {
		return t.Native(ctx, c.Name, c.Args, t.WS, t.UID)
	}
	var res string
	var handled bool
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		res, handled, err = ExecNative(ctx, tx, c.Name, c.Args, t.WS, t.UID)
		return err
	})
	return res, handled, err
}

func (t *Turn) pythonTool(ctx context.Context, c parsedCall) (string, error) {
	if t.PY == nil {
		return toolErr(c.Name, "worker error"), nil // the engine runs without a worker; this tool has no Go port
	}
	uid, err := t.Auth.ToolUserID()
	if err != nil || t.Auth.WorkspaceID != t.WS {
		if err == nil {
			err = errors.New("copilot: auth workspace differs from the turn workspace")
		}
		return "", &turnErr{errTextTools, err}
	}
	timeout := t.toolTimeout()
	body := map[string]any{"name": c.Name, "args": c.Args, "workspace_id": t.WS, "user_id": uid, "slug": t.Slug,
		"timeout_s": min(timeout, 120*time.Second).Seconds()}
	var out struct {
		Result *string `json:"result"`
	}
	call := t.PY.Unary
	if IsDangerous(c.Name) {
		call = t.PY.UnaryOnce // never re-send an approval-gated tool: a retry on a fresh worker could run it twice
	}
	err = call(ctx, "/internal/copilot/tool", body, &out, timeout+unarySlack)
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.Is(err, pyworker.ErrTimeout):
		return toolErr(c.Name, "timed out"), nil
	case errors.Is(err, pyworker.ErrUnknownProvider):
		return `{"error": ` + jstr("Unknown tool: "+c.Name) + `}`, nil
	case err != nil:
		t.logger().Warn("copilot python tool failed", "tool", c.Name, "reason", err.Error())
		return toolErr(c.Name, "worker error"), nil
	case out.Result == nil:
		return toolErr(c.Name, "worker error"), nil
	}
	return *out.Result, nil
}

// pairs is an object's members in source order with raw values.
type pair struct {
	k string
	v json.RawMessage
}

func objectPairs(raw []byte) ([]pair, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("copilot: not an object")
	}
	var out []pair
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, pair{kt.(string), v})
	}
	return out, nil
}

func renderPairs(ps []pair) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range ps {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(jstr(p.k))
		b.WriteString(": ")
		b.Write(p.v)
	}
	b.WriteByte('}')
	return b.Bytes()
}
