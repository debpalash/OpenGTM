package copilot

import (
	"context"
	"errors"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

func TestFinishFaultWritesFixedFrameAndDone(t *testing.T) {
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	tn := &Turn{}
	tn.FinishFault(context.Background(), w, &turnErr{errTextApproval, errors.New("pq: secret detail")})
	want := "data: {\"error\": \"" + errTextApproval + "\"}\n\ndata: [DONE]\n\n"
	if rec.String() != want {
		t.Fatalf("got %q", rec.String())
	}
	rec = newRec()
	w, _ = ssestream.New(rec, nil)
	tn.FinishFault(context.Background(), w, errors.New("anything else"))
	if rec.String() != "data: {\"error\": \""+errTextInterrupted+"\"}\n\ndata: [DONE]\n\n" {
		t.Fatalf("generic: %q", rec.String())
	}
}

func TestFinishFaultSilentWhenClientGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	(&Turn{}).FinishFault(ctx, w, &turnErr{errTextTools, errors.New("x")})
	if rec.String() != "" {
		t.Fatalf("wrote %q", rec.String())
	}
}
