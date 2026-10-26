package copilot

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

const (
	// maxApproved caps one resubmit; every item costs a DB write and possibly a tool run.
	maxApproved = 20
	// approvalTTL is Python's consume_tool_approval window.
	approvalTTL = time.Hour
)

// MaxApproved is the largest approved_tool_calls list ResolveApproved accepts; callers check it before
// the response starts so an oversized list can be answered with a status.
const MaxApproved = maxApproved

// FinishFault ends a turn whose ResolveApproved failed: a *turnErr becomes its fixed error frame and [DONE],
// any other failure the generic interrupted frame; a cancelled context writes nothing (the client is gone).
func (t *Turn) FinishFault(ctx context.Context, w *ssestream.Writer, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	text := errTextInterrupted
	var te *turnErr
	if errors.As(err, &te) {
		text = te.text
	}
	t.logger().Warn("copilot approval resume ended with an error frame", "conversation", t.ConvID, "reason", err.Error())
	if t.emitKey(w, "error", []byte(jstr(text))) == nil {
		_ = w.Done()
	}
}

// ErrTooManyApprovals is returned, before anything is consumed, when approved_tool_calls has more than 20 items.
var ErrTooManyApprovals = errors.New("copilot: too many approved tool calls")

// Approved is one approved_tool_calls item: the approval id the client echoed (tool_call.id) and its
// decision. The caller applies Python's default ("approve" when the key is absent); an empty or other
// decision is never usable.
type Approved struct {
	ID       string
	Decision string
}

// resolved is one item after consumption; call is nil for an unusable approval.
type resolved struct {
	id, decision string
	call         map[string]any
	name         string
	args         string
}

// ResolveApproved is _resolve_approved_calls. Every approval is consumed (one transaction, committed)
// BEFORE anything runs, so a concurrent resubmit of the same id can never execute a call twice. The
// messages (one assistant message carrying every call, then one tool message per call) are appended to
// *messages and the SSE events written to w. A returned error is fatal for the turn (cancelled context,
// no tool user, DB failure as *turnErr).
func (t *Turn) ResolveApproved(ctx context.Context, w *ssestream.Writer, messages *[]map[string]any, approved []Approved) error {
	if len(approved) == 0 {
		return nil
	}
	if len(approved) > maxApproved {
		return ErrTooManyApprovals
	}
	items := make([]resolved, len(approved))
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		for i, a := range approved {
			items[i] = resolved{id: a.ID, decision: a.Decision}
			call, ok, err := chathistory.ConsumeApproval(ctx, tx, a.ID, t.WS, t.UID, a.Decision, approvalTTL, time.Now())
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			name, args, ok := splitStoredCall(call)
			if !ok {
				continue // unusable stored row: burned and treated as invalid
			}
			items[i].call, items[i].name, items[i].args = call, name, args
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &turnErr{errTextApproval, err}
	}

	calls := make([]any, len(items))
	for i, it := range items {
		if it.call != nil {
			calls[i] = it.call
			continue
		}
		id := it.id
		if id == "" {
			id = "invalid-" + itoa(i)
		}
		calls[i] = map[string]any{"id": id, "type": "function",
			"function": map[string]any{"name": "invalid_approval", "arguments": "{}"}}
	}
	*messages = append(*messages, map[string]any{"role": "assistant", "content": "", "tool_calls": calls})

	for _, it := range items {
		if it.call == nil {
			if err := t.denied(w, messages, "invalid_approval", orStr(it.id, "invalid-approval"), invalidApprovalReason); err != nil {
				return err
			}
			continue
		}
		callID, _ := it.call["id"].(string)
		c := newApprovedCall(callID, it.name, it.args)
		if it.decision == "approve" && IsDangerous(it.name) {
			content, err := t.executeApproved(ctx, w, c)
			if err != nil {
				return err
			}
			*messages = append(*messages, map[string]any{"role": "tool", "tool_call_id": callID, "name": it.name, "content": content})
			continue
		}
		reason := "Tool is not an explicitly classified mutation."
		if it.decision == "deny" {
			reason = "User declined this action."
		}
		if err := t.denied(w, messages, it.name, callID, reason); err != nil {
			return err
		}
	}
	return nil
}

const invalidApprovalReason = "Approval is invalid, expired, already consumed, or belongs to another workspace."

func itoa(i int) string { return pyStr(i) }

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (t *Turn) denied(w *ssestream.Writer, messages *[]map[string]any, name, callID, reason string) error {
	if err := t.emit(w, []byte(`{"tool_denied": {"name": `+jstr(name)+`}}`)); err != nil {
		return err
	}
	*messages = append(*messages, map[string]any{"role": "tool", "tool_call_id": callID, "name": name,
		"content": `{"denied": true, "reason": ` + jstr(reason) + `}`})
	return nil
}

// splitStoredCall reads the function name and arguments string of a stored call.
func splitStoredCall(call map[string]any) (name, args string, ok bool) {
	fn, _ := call["function"].(map[string]any)
	name, _ = fn["name"].(string)
	if name == "" {
		return "", "", false
	}
	switch a := fn["arguments"].(type) {
	case nil:
	case string:
		args = a
	default:
		return "", "", false
	}
	return name, args, true
}

// newApprovedCall decodes arguments like json.loads(arguments or "{}") with {} on any failure.
func newApprovedCall(id, name, argStr string) parsedCall {
	c := parsedCall{ID: id, Name: name, ArgsStr: argStr, ArgsJSON: []byte("{}"), Args: map[string]any{}}
	src := argStr
	if src == "" {
		src = "{}"
	}
	if v, err := canonjson.Decode([]byte(src)); err == nil {
		if m, ok := v.(map[string]any); ok {
			if aj, err := ssestream.Reformat([]byte(src)); err == nil {
				c.Args, c.ArgsJSON = m, aj
			}
		}
	}
	return c
}

// executeApproved runs one approved dangerous call (already consumed) and writes tool_call, tool_result
// and the per-tool follow-up content. It returns the raw tool result for the tool message.
func (t *Turn) executeApproved(ctx context.Context, w *ssestream.Writer, c parsedCall) (string, error) {
	if err := t.emit(w, []byte(`{"tool_call": {"name": `+jstr(c.Name)+`, "args": `+string(c.ArgsJSON)+`}}`)); err != nil {
		return "", err
	}
	result, err := t.awaitApproved(ctx, w, c)
	if err != nil {
		return "", err
	}
	if err := t.emit(w, []byte(`{"tool_result": {"name": `+jstr(c.Name)+`, "result": `+string(toolResultValue(result))+`}}`)); err != nil {
		return "", err
	}
	parsed := map[string]any{}
	if v, err := canonjson.Decode([]byte(result)); err == nil {
		if m, ok := v.(map[string]any); ok {
			parsed = m
		}
	} else {
		parsed = map[string]any{"raw": result}
	}
	if msg, ok := followUp(c.Name, parsed); ok {
		if err := t.emit(w, []byte(`{"content": `+jstr(msg)+`}`)); err != nil {
			return "", err
		}
	}
	return result, nil
}

// awaitApproved is await with the execution-time role re-check in front: the single permission decision
// (identity.HasPermissionTx, tables.write) at the moment of execution, not at proposal time. Self-host
// (nil user) skips it like Python.
func (t *Turn) awaitApproved(ctx context.Context, w *ssestream.Writer, c parsedCall) (string, error) {
	if t.UID != nil {
		var ok bool
		err := t.inTx(ctx, func(tx pgx.Tx) error {
			var err error
			ok, err = identity.HasPermissionTx(ctx, tx, t.WS, *t.UID, "tables.write", "admin", "editor")
			return err
		})
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case err != nil:
			t.logger().Warn("copilot role check failed", "tool", c.Name)
			return toolErr(c.Name, "internal error"), nil
		case !ok:
			return errInsufficientRole, nil
		}
	}
	return t.await(ctx, w, c)
}

// followUp is the chat message Python adds after specific tools.
func followUp(name string, r map[string]any) (string, bool) {
	switch name {
	case "create_people_workbook":
		if pyTruthy(r["workbook_id"]) {
			return "Created **" + get(r, "name", "People workbook") + "** with **" + get(r, "total_rows", "0") +
				" people**. [Open workbook](" + pyStr(r["url"]) + ")", true
		}
	case "enrich_people_contacts":
		if pyTruthy(r["ok"]) {
			return FormatPeopleContacts(r), true
		}
	case "create_source_workbook":
		if pyTruthy(r["workbook_id"]) {
			return "Created **" + get(r, "name", "Sourcing workbook") + "**. The source job is " +
				get(r, "source_job_id", "not started") + ". [Open workbook](" + pyStr(r["url"]) + ")", true
		}
	case "track_account_signals":
		return FormatSignalTracking(r), true
	case "draft_grounded_outreach":
		return FormatGroundedDraft(r), true
	}
	return "", false
}
