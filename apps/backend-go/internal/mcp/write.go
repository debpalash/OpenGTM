package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

const (
	defaultMaxInFlightWrites = 8
	maxIdempotencyKeyLen     = 128
)

// errStructured marks a write whose result carried an "error" key: the
// transaction rolls back (D5) and the text is returned to the client.
type errStructured struct{ text, msg string }

func (e *errStructured) Error() string { return e.msg }

// writeOutcome is what the write transaction decided.
type writeOutcome int

const (
	woApplied writeOutcome = iota
	woReplay
	woCapped
)

func (s *Server) acquireWriteSlot(ws string) bool {
	max := s.MaxInFlightWrites
	if max <= 0 {
		max = defaultMaxInFlightWrites
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight == nil {
		s.inflight = map[string]int{}
	}
	if s.inflight[ws] >= max {
		return false
	}
	s.inflight[ws]++
	return true
}

func (s *Server) releaseWriteSlot(ws string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight[ws] <= 1 {
		delete(s.inflight, ws)
		return
	}
	s.inflight[ws]--
}

func denied(s *Server, ctx context.Context, c Ctx, name string, args Args, reason string) string {
	Record(ctx, s.d, c, name, args, AuditOutcome{Status: "denied", Error: reason})
	return errorText(reason)
}

// executeWrite is _execute_write: key validation, tool gate, live role check,
// then ONE transaction holding Reserve, Apply and Settle (D5). The audit row is
// written after the transaction, once per attempt.
func (s *Server) executeWrite(ctx context.Context, c Ctx, name string, args Args) string {
	wt, ok := s.tools.Writes[name]
	if !ok || wt.Apply == nil {
		return errorText(ErrUnavailable.Error())
	}
	key := ""
	if v, present := args["idempotency_key"]; present && v != nil {
		k, isStr := v.(string)
		if !isStr || utf8.RuneCountInString(k) > maxIdempotencyKeyLen {
			msg := "idempotency_key must be a string up to 128 characters"
			Record(ctx, s.d, c, name, args, AuditOutcome{Status: "error", Error: msg})
			return errorText(msg)
		}
		key = k
	}
	if !s.acquireWriteSlot(c.WorkspaceID) {
		return denied(s, ctx, c, name, args, "too many concurrent writes")
	}
	defer s.releaseWriteSlot(c.WorkspaceID)

	if wt.Gate != nil {
		if reason := wt.Gate(s.d.Cfg); reason != "" {
			return denied(s, ctx, c, name, args, reason)
		}
	}
	if err := RequireRole(ctx, s.d, c, wt.Roles...); err != nil {
		var ae *AuthError
		if errors.As(err, &ae) {
			return denied(s, ctx, c, name, args, ae.Msg)
		}
		Record(ctx, s.d, c, name, args, AuditOutcome{Status: "error", Error: errCause(ctx, err)})
		return errorText(errCause(ctx, err))
	}

	var (
		outcome writeOutcome
		res     Reservation
		result  string
	)
	err := s.d.RunInWorkspace(ctx, c.WorkspaceID, func(tx pgx.Tx) error {
		r, err := Reserve(ctx, tx, c.WorkspaceID, WriteRule, s.d.Cfg.MaxWritesPerDay, key)
		if err != nil {
			return err
		}
		res = r
		if r.Replay {
			outcome = woReplay
			return nil
		}
		if !r.OK {
			outcome = woCapped
			return nil
		}
		text, err := applyRecovered(ctx, tx, wt, c, args)
		if err != nil {
			return err
		}
		if msg, isErr := resultError(text); isErr {
			return &errStructured{text: text, msg: msg}
		}
		if err := Settle(ctx, tx, r); err != nil {
			return err
		}
		// A timed out or cancelled call must not commit after the client was told.
		if err := ctx.Err(); err != nil {
			return err
		}
		result = text
		outcome = woApplied
		return nil
	})

	if err != nil {
		var es *errStructured
		if errors.As(err, &es) {
			Record(ctx, s.d, c, name, args, AuditOutcome{Status: "error", Error: es.msg})
			return es.text
		}
		msg := errCause(ctx, err)
		Record(ctx, s.d, c, name, args, AuditOutcome{Status: "error", Error: msg})
		return errorText(msg)
	}
	switch outcome {
	case woReplay:
		// No key on the row: audit keys are unique per workspace and the original
		// applied write already holds it (Python records the replay with a null key).
		Record(ctx, s.d, c, name, args, AuditOutcome{Status: "ok", Error: "idempotent_replay"})
		b, _ := pyjson.Dumps(pyjson.NewObj("status", "duplicate", "idempotency_key", res.Key,
			"message", "already applied (idempotent replay)"), 0)
		return string(b)
	case woCapped:
		Record(ctx, s.d, c, name, args, AuditOutcome{Status: "capped", Error: "daily MCP write cap exceeded"})
		return errorText("mcp write cap exceeded")
	}
	Record(ctx, s.d, c, name, args, AuditOutcome{Status: "ok", IdempotencyKey: res.Key})
	s.postCommit(ctx, c, name, result)
	return result
}

// applyRecovered runs Apply and turns a panic into an error (the transaction
// then rolls back); the panic text is logged, never returned or audited.
func applyRecovered(ctx context.Context, tx pgx.Tx, wt WriteTool, c Ctx, args Args) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mcp write tool panic", "tool", wt.Name, "panic", fmt.Sprint(r))
			text, err = "", errInternal
		}
	}()
	return wt.Apply(ctx, tx, c, args)
}

var errInternal = errors.New("internal error")

// errCause renders a failed transaction for the client and the audit row:
// context errors say so, safe typed errors keep their text, the rest is masked.
func errCause(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "tool timed out"
	case errors.Is(err, context.Canceled):
		return "request cancelled"
	case ctx.Err() != nil:
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "tool timed out"
		}
		return "request cancelled"
	}
	return safeErrorText(err)
}

// resultError reports whether text is a JSON object with an "error" key (the
// Python _result_is_error rule) and returns that error's text.
func resultError(text string) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &m) != nil {
		return "", false
	}
	raw, ok := m["error"]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return string(raw), true
}

// postCommit runs best-effort steps after a committed write: bootstrap the
// schedule of an enabled on_schedule automation. Errors are logged only.
func (s *Server) postCommit(ctx context.Context, c Ctx, name, result string) {
	if name != "create_automation" {
		return
	}
	var r struct {
		AutomationID string `json:"automation_id"`
		Automation   struct {
			TriggerType string `json:"trigger_type"`
			Enabled     bool   `json:"enabled"`
		} `json:"automation"`
	}
	if json.Unmarshal([]byte(result), &r) != nil || r.AutomationID == "" ||
		r.Automation.TriggerType != "on_schedule" || !r.Automation.Enabled {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Warn("mcp create_automation schedule bootstrap panicked")
		}
	}()
	// The hook is best effort after the commit: it must not be cut short by the
	// request deadline that follows a slow call.
	if err := s.d.AutomationPort().BootstrapSchedule(context.WithoutCancel(ctx), c.WorkspaceID, r.AutomationID); err != nil {
		slog.Warn("mcp create_automation schedule bootstrap failed", "err", err.Error())
	}
}
