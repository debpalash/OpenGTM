package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilotauth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Fixed texts of the Go-built error frames. Python and provider error strings never reach the client
// through these, so no secret or driver text can leak.
const (
	errTextInterrupted = "AI provider stream interrupted"
	errTextTimeout     = "Request timed out"
	errTextTooMany     = "Too many tool calls requested"
	errTextTools       = "Tool execution unavailable"
	errTextApproval    = "Could not record approval request"
)

const (
	defaultKeepalive   = 10 * time.Second // Python SSE_KEEPALIVE_SECONDS
	defaultToolTimeout = 120 * time.Second
	defaultMaxRounds   = 8
	maxToolResultBytes = 1 << 20
)

// maxTurnToolBytes is the aggregate size of the tool results one turn may feed back to the model (variable so
// tests can lower it): CHAT_MAX_TOOL_ROUNDS has no upper bound, so the per-result cap alone does not bound the prompt.
var maxTurnToolBytes = 4 << 20

// Settings is the app-settings getter (settings.App satisfies it).
type Settings interface {
	Get(ctx context.Context, key, def string) (string, error)
}

// NativeFunc executes a tool. handled=false means "not answered here".
type NativeFunc func(ctx context.Context, name string, args map[string]any, ws string, uid *int32) (result string, handled bool, err error)

// Deps are the turn's collaborators.
type Deps struct {
	Pool     *pgxpool.Pool
	Settings Settings // nil: defaults
	// Engine runs the llm rounds, pre-routes, action descriptions and the engine tools. A turn without one
	// runs no pre-route, describes actions from the catalog and fails its llm round (unless Rounds is set).
	Engine *Engine
	// Rounds overrides how llm rounds run (tests); nil runs them on Engine.LLM.
	Rounds RoundFunc
	// Native, when set, is consulted first for every tool (tests); handled=false continues with the Go
	// implementations (ExecNative, then the engine tools).
	Native NativeFunc
	// NewTicker overrides the keepalive clock (tests); nil uses time.NewTicker.
	NewTicker func(time.Duration) (<-chan time.Time, func())
	Keepalive time.Duration // default 10 s
	// ToolTimeout bounds one tool (COPILOT_TOOL_TIMEOUT_SECONDS, default 120 s); TurnTimeout bounds the
	// whole turn (COPILOT_TURN_TIMEOUT_SECONDS; 0 means the caller's context only).
	ToolTimeout time.Duration
	TurnTimeout time.Duration
	Log         *slog.Logger
}

// Turn is one chat turn (_stream_chat). It is not safe for concurrent use.
type Turn struct {
	Deps
	WS, Slug string
	UID      *int32
	ConvID   string
	// Auth is the resolved tenant; its ToolUserID owns the turn's approvals and a Resolved that
	// cannot name a tool user fails the turn closed.
	Auth copilotauth.Resolved
	// Seen is the per-turn dedup cache (name|sorted-args -> result). Created on first use.
	Seen map[string]string
	// OpenFault, when set, reports that the HTTP response has not started. PreRoute then returns
	// ErrPreRouteOpen, writing nothing, for a fault before its first frame, so the caller can answer 5xx.
	OpenFault func() bool
	// OnEvent, when set, receives the exact JSON of every data frame just before it is written (persistence).
	OnEvent func(raw []byte)
	// anthropicRaw keeps a native Anthropic tool-use turn's content blocks by first tool-call id, so the next
	// round replays them unchanged (thinking blocks must come back as produced).
	anthropicRaw map[string][]json.RawMessage
	// toolBytes is the running size of the tool results sent to the model this turn (see maxTurnToolBytes).
	toolBytes int
}

// errBadFrame marks a frame whose JSON could not be re-serialised.
var errBadFrame = errors.New("copilot: malformed frame")

// turnErr ends the turn with one fixed error frame and [DONE].
type turnErr struct {
	text string
	err  error
}

func (e *turnErr) Error() string { return e.err.Error() }
func (e *turnErr) Unwrap() error { return e.err }

func (t *Turn) logger() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}

func (t *Turn) keepaliveEvery() time.Duration {
	if t.Keepalive > 0 {
		return t.Keepalive
	}
	return defaultKeepalive
}

func (t *Turn) toolTimeout() time.Duration {
	if t.ToolTimeout > 0 {
		return t.ToolTimeout
	}
	return defaultToolTimeout
}

func (t *Turn) ticker() (<-chan time.Time, func()) {
	if t.NewTicker != nil {
		return t.NewTicker(t.keepaliveEvery())
	}
	tk := time.NewTicker(t.keepaliveEvery())
	return tk.C, tk.Stop
}

// maxRounds is CHAT_MAX_TOOL_ROUNDS: default 8, floor 1, invalid -> 8; read once per turn.
func (t *Turn) maxRounds(ctx context.Context) int {
	if t.Settings == nil {
		return defaultMaxRounds
	}
	v, err := t.Settings.Get(ctx, "CHAT_MAX_TOOL_ROUNDS", "8")
	if err != nil {
		return defaultMaxRounds
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return defaultMaxRounds
	}
	return max(1, n)
}

// emit writes one data frame given its JSON (any spacing; Reformat renders it like Python's json.dumps).
func (t *Turn) emit(w *ssestream.Writer, payload []byte) error {
	r, err := ssestream.Reformat(payload)
	if err != nil {
		return fmt.Errorf("%w: %v", errBadFrame, err)
	}
	if t.OnEvent != nil {
		t.OnEvent(r)
	}
	return w.EventRaw(r)
}

// emitObj writes {"<key>": <raw json value>}.
func (t *Turn) emitKey(w *ssestream.Writer, key string, rawValue []byte) error {
	return t.emit(w, []byte(`{`+jstr(key)+`: `+string(rawValue)+`}`))
}

// Run is _stream_chat: it drives llm rounds, runs safe tools inline and gates everything else. The HTTP
// status is already 200 when Run starts, so failures become one fixed error frame and [DONE]. The returned
// error is informational (logging) once frames were written; a context error means the client went away
// or the caller cancelled and nothing more was written.
func (t *Turn) Run(ctx context.Context, w *ssestream.Writer, messages []map[string]any) error {
	outer := ctx
	if t.TurnTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.TurnTimeout)
		defer cancel()
	}
	if t.Seen == nil {
		t.Seen = map[string]string{}
	}
	err := t.loop(ctx, w, append([]map[string]any(nil), messages...))
	if err == nil {
		return nil
	}
	var te *turnErr
	switch {
	case errors.As(err, &te):
		t.logger().Warn("copilot turn ended with an error frame", "conversation", t.ConvID, "reason", fmt.Sprintf("%T", te.err))
		_ = t.emitKey(w, "error", []byte(jstr(te.text)))
		_ = w.Done()
	case outer.Err() != nil && !errors.Is(outer.Err(), context.DeadlineExceeded):
		return outer.Err() // the client went away (a caller-owned deadline falls through to the timeout frame)
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		_ = t.emitKey(w, "error", []byte(jstr(errTextTimeout)))
		_ = w.Done()
	}
	return err
}

func (t *Turn) loop(ctx context.Context, w *ssestream.Writer, msgs []map[string]any) error {
	maxRounds := t.maxRounds(ctx)
	req := RoundRequest{Messages: msgs, ToolsEnabled: true}
	for roundIdx := 0; ; {
		res, err := t.round(ctx, w, req)
		if err != nil {
			return err
		}
		if !res.hasCalls {
			return nil
		}
		req.Cursor = res.cursor
		calls, err := parseToolCalls(res.calls)
		if err != nil {
			return &turnErr{errTextInterrupted, err}
		}
		var dangerous []parsedCall
		for _, c := range calls {
			if c.Dangerous {
				dangerous = append(dangerous, c)
			}
		}
		toolMsgs, err := t.runSafe(ctx, w, calls)
		if err != nil {
			return err
		}
		if len(dangerous) > 0 {
			return t.gate(ctx, w, dangerous)
		}
		follow := append(append([]map[string]any(nil), req.Messages...),
			map[string]any{"role": "assistant", "content": "", "tool_calls": callsAny(calls)})
		follow = append(follow, toolMsgs...)
		roundIdx++
		if roundIdx >= maxRounds {
			follow = append(follow, map[string]any{"role": "system", "content": "You have reached the maximum of " +
				strconv.Itoa(maxRounds) + " tool-use rounds. Do NOT call any more tools. Summarize what you found and the next step the user can take."})
			req.ToolsEnabled, req.BudgetHit = false, true
		}
		req.Messages = follow
	}
}

// inTx runs fn in the workspace transaction.
func (t *Turn) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if t.Pool == nil {
		return errors.New("copilot: no database pool")
	}
	return tenancy.InWorkspace(ctx, t.Pool, t.WS, fn)
}
