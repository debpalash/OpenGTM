package copilot

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// ErrPreRouteOpen is returned by PreRoute, with nothing written, when Turn.OpenFault reports that the HTTP
// response has not started and the pre-route failed before producing any output. Python pulls the first
// frame before building the response, so there such a fault fails the request; the caller answers 5xx.
var ErrPreRouteOpen = errors.New("copilot: pre-route unavailable")

// PreRoute runs the deterministic pre-route branches (services/copilot/preroute.py) on the engine. It returns
// handled=false, having written nothing, when no branch answers; the caller then continues with the LLM path.
// A turn without an engine has no pre-route. A branch the engine cannot answer (the people branches without
// People) ends the stream with one fixed error frame and [DONE].
func (t *Turn) PreRoute(ctx context.Context, w *ssestream.Writer, lastUserMsg string, hasApproved bool) (bool, error) {
	if _, err := t.Auth.ToolUserID(); err != nil { // approvals are created in this (workspace, user); fail closed like tools
		return true, t.preFail(w, &turnErr{errTextTools, err})
	}
	if t.Engine == nil {
		return false, nil
	}
	handled, delegate, err := t.nativePreRoute(ctx, w, lastUserMsg, hasApproved)
	if delegate {
		if t.OpenFault != nil && t.OpenFault() {
			return false, fmt.Errorf("%w: %v", ErrPreRouteOpen, errPreRouteUnported)
		}
		return true, t.preFail(w, &turnErr{errTextInterrupted, errPreRouteUnported})
	}
	return handled, err
}

// errPreRouteUnported marks a pre-route branch the engine cannot answer (no People collaborator).
var errPreRouteUnported = errors.New("copilot: pre-route branch needs the people service")

// storeMessage appends one history row in its own transaction (rows of one turn need distinct created_at).
// Failures are logged without the content and swallowed.
func (t *Turn) storeMessage(ctx context.Context, role, content string, toolData *string) {
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		return chathistory.AddMessage(ctx, tx, t.ConvID, role, content, toolData, t.WS, t.UID)
	})
	if err != nil {
		t.logger().Warn("copilot history write failed", "conversation", t.ConvID, "role", role, "reason", fmt.Sprintf("%T", err))
	}
}

// preFail writes the fixed error frame and [DONE] and returns the turn error for logging.
func (t *Turn) preFail(w *ssestream.Writer, te *turnErr) error {
	t.logger().Warn("copilot preroute ended with an error frame", "conversation", t.ConvID, "reason", fmt.Sprintf("%T", te.err))
	if err := t.emitKey(w, "error", []byte(jstr(te.text))); err == nil {
		_ = w.Done()
	}
	return te
}
