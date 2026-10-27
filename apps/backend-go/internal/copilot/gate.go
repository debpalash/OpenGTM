package copilot

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// gate is the human-in-the-loop gate: no dangerous call is executed. Each gets one approval row (created and
// committed before any event names its id, so a fast resubmit finds it), one confirmation_required event, then
// awaiting_confirmation and [DONE] end the turn. The client resubmits approved_tool_calls (Task 14).
func (t *Turn) gate(ctx context.Context, w *ssestream.Writer, dangerous []parsedCall) error {
	// Python has no cap; a steered model could otherwise flood describe RPCs and approval rows while holding
	// the stream slot. Same ceiling as the approved-call list a client may resubmit.
	if len(dangerous) > MaxApproved {
		return &turnErr{errTextTooMany, errors.New("copilot: too many dangerous tool calls in one round")}
	}
	descs := make([]string, len(dangerous))
	for i, c := range dangerous {
		descs[i] = t.describe(ctx, c)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	ids := make([]string, len(dangerous))
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		for i, c := range dangerous {
			id, err := chathistory.CreateApproval(ctx, tx, t.WS, t.UID, c.tc)
			if err != nil {
				return err
			}
			ids[i] = id
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &turnErr{errTextApproval, err}
	}
	for i, c := range dangerous {
		display, err := displayCall(c, ids[i])
		if err != nil {
			return &turnErr{errTextInterrupted, err}
		}
		level, label := Meta(c.Name)
		ev := `{"confirmation_required": ` + string(renderPairs([]pair{
			{"confirmation_id", []byte(jstr(ids[i]))},
			{"tool_call", display},
			{"name", []byte(jstr(c.Name))},
			{"args", c.ArgsJSON},
			{"description", []byte(jstr(descs[i]))},
			{"level", []byte(jstr(level))},
			{"label", []byte(jstr(label))},
		})) + `}`
		if err := t.emit(w, []byte(ev)); err != nil {
			return err
		}
	}
	if err := t.emit(w, []byte(`{"awaiting_confirmation": true}`)); err != nil {
		return err
	}
	return w.Done()
}

// displayCall is json.loads(json.dumps(tc)) with id replaced by the approval id, member order kept, and the
// "{}" arguments fix applied.
func displayCall(c parsedCall, approvalID string) ([]byte, error) {
	ps, err := objectPairs(c.raw)
	if err != nil {
		return nil, err
	}
	hasID := false
	for i := range ps {
		switch ps[i].k {
		case "id":
			ps[i].v, hasID = []byte(jstr(approvalID)), true
		case "function":
			fps, err := objectPairs(ps[i].v)
			if err != nil {
				return nil, errors.New("copilot: tool call function is not an object")
			}
			hasArgs := false
			for j := range fps {
				if fps[j].k == "arguments" {
					hasArgs = true
					if c.ArgsStr == "" {
						fps[j].v = []byte(`"{}"`)
					}
				}
			}
			if !hasArgs {
				fps = append(fps, pair{"arguments", []byte(`"{}"`)})
			}
			ps[i].v = renderPairs(fps)
		}
	}
	if !hasID {
		ps = append(ps, pair{"id", []byte(jstr(approvalID))})
	}
	return renderPairs(ps), nil
}

// describe is the confirmation card text: the worker's describe_action, or Python's own fallback
// "label\nreason" when the worker cannot answer (a card must still be shown; the gate never depends on it).
func (t *Turn) describe(ctx context.Context, c parsedCall) string {
	if t.Engine != nil && c.Name == "execute_plan" {
		return t.describePlan(ctx, c.Args)
	}
	if t.Engine != nil {
		if d, ok := DescribeAction(c.Name, c.Args); ok {
			return d
		}
	}
	var out struct {
		Description *string `json:"description"`
	}
	if t.PY == nil {
		return t.describeFallback(c.Name)
	}
	err := t.PY.Unary(ctx, "/internal/copilot/describe",
		map[string]any{"name": c.Name, "args": c.Args, "workspace_id": t.WS}, &out, describeTimeout)
	if err == nil && out.Description != nil {
		return *out.Description
	}
	if ctx.Err() == nil {
		t.logger().Warn("copilot describe failed", "tool", c.Name)
	}
	return t.describeFallback(c.Name)
}

// describeFallback is Python's own "label\nreason" card text, used when no description can be rendered.
func (t *Turn) describeFallback(name string) string {
	label, reason := name, "This action modifies data."
	for _, tool := range Catalog() {
		if tool.Name == name && tool.Label != "" {
			label = tool.Label
			if tool.Reason != "" {
				reason = tool.Reason
			}
		}
	}
	return label + "\n" + reason
}
