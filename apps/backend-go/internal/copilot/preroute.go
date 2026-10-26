package copilot

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// ErrPreRouteOpen is returned by PreRoute, with nothing written, when Turn.OpenFault reports that the HTTP
// response has not started and the pre-route failed before producing any frame. Python pulls the first
// frame before building the response, so there such a fault fails the request; the caller answers 5xx.
var ErrPreRouteOpen = errors.New("copilot: pre-route unavailable")

// errWorkerError marks a terminal error frame from the worker. Its message is never forwarded or logged.
var errWorkerError = errors.New("copilot: preroute worker reported an error")

type preItem struct {
	f   pyworker.Frame
	err error
}

// PreRoute asks the Python worker whether one of the deterministic pre-route branches answers the
// message (the first-frame pull of copilot_chat). It returns handled=false, having written nothing, when
// the worker says "none"; the caller then continues with the LLM path. Otherwise the branch's frames are
// relayed: sse frames re-emitted byte-exact through Reformat, persist frames written to the history (Go is
// the single writer; a failed write is logged without content and never breaks the stream), keepalive
// frames dropped (Go owns the 10 s timer) and done writes [DONE].
//
// A worker or transport fault ends the stream with one fixed error frame and [DONE], returns handled=true
// and an informational error. A cancelled context returns its error and writes nothing more.
func (t *Turn) PreRoute(ctx context.Context, w *ssestream.Writer, lastUserMsg string, hasApproved bool) (bool, error) {
	uid, err := t.Auth.ToolUserID() // approvals are created in this (workspace, user); fail closed like tools
	if err != nil {
		return true, t.preFail(w, &turnErr{errTextTools, err})
	}
	seen := false // a frame reached us: the stream is no longer a clean open failure
	fail := func(te *turnErr) error {
		if !seen && t.OpenFault != nil && t.OpenFault() {
			t.logger().Warn("copilot preroute failed before any frame", "conversation", t.ConvID, "reason", te.err.Error())
			return fmt.Errorf("%w: %v", ErrPreRouteOpen, te.err)
		}
		return t.preFail(w, te)
	}
	body := map[string]any{
		"workspace_id": t.WS, "slug": t.Slug, "conversation_id": t.ConvID,
		"last_user_message": lastUserMsg, "has_approved_calls": hasApproved,
	}
	if uid == nil {
		body["user_id"] = nil
	} else {
		body["user_id"] = *uid
	}
	st, err := t.PY.Stream(ctx, "/internal/copilot/preroute", body)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return true, fail(&turnErr{errTextInterrupted, err})
	}
	defer st.Close()

	items := make(chan preItem)
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			f, err := st.Next()
			select {
			case items <- preItem{f, err}:
			case <-quit:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		close(quit)
		st.Close()
		<-finished
	}()

	tick, stop := t.ticker()
	defer stop()
	wrote := false
	for {
		select {
		case <-ctx.Done():
			return wrote, ctx.Err()
		case <-tick:
			if err := w.Keepalive(); err != nil {
				return true, err
			}
			// a keepalive is an SSE comment, not output: a slow "none" must still fall through to the LLM
		case it := <-items:
			if it.err != nil {
				if ctx.Err() != nil {
					return wrote, ctx.Err()
				}
				return true, fail(&turnErr{errTextInterrupted, it.err})
			}
			if it.f.T == "sse" || it.f.T == "persist" {
				seen = true
			}
			done, handled, err := t.preFrame(ctx, w, it.f, &wrote)
			if err != nil {
				if ctx.Err() != nil {
					return wrote, ctx.Err()
				}
				var te *turnErr
				if errors.As(err, &te) {
					return true, fail(te)
				}
				return true, err // the client's connection is gone
			}
			if done {
				return handled, nil
			}
		}
	}
}

// preFrame handles one worker frame. done=true ends the relay with the given handled result.
func (t *Turn) preFrame(ctx context.Context, w *ssestream.Writer, f pyworker.Frame, wrote *bool) (done, handled bool, err error) {
	switch f.T {
	case "sse":
		d := []byte(f.D)
		if len(d) == 0 || string(d) == "null" {
			return false, false, &turnErr{errTextInterrupted, errBadFrame}
		}
		if err := t.emit(w, d); err != nil {
			return false, false, t.relayErr(ctx, err)
		}
		*wrote = true
	case "persist":
		t.persistFrame(ctx, f)
	case "done":
		if err := w.Done(); err != nil {
			return false, false, err
		}
		return true, true, nil
	case "none":
		// Python's router ignores a late none and just ends the response; it only means "no branch" first.
		return true, *wrote, nil
	case "error":
		return false, false, &turnErr{errTextInterrupted, errWorkerError}
	}
	return false, false, nil // keepalive and unknown frame types
}

func (t *Turn) persistFrame(ctx context.Context, f pyworker.Frame) {
	if f.Role == nil || f.Content == nil {
		t.logger().Warn("copilot preroute persist frame is incomplete", "conversation", t.ConvID)
		return
	}
	t.storeMessage(ctx, *f.Role, *f.Content, f.ToolData)
}

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
	t.logger().Warn("copilot preroute ended with an error frame", "conversation", t.ConvID, "reason", te.err.Error())
	if err := t.emitKey(w, "error", []byte(jstr(te.text))); err == nil {
		_ = w.Done()
	}
	return te
}
