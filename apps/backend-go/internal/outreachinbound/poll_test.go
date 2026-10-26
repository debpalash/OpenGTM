package outreachinbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/imapclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

type vectors struct {
	Backoff []struct {
		Interval int `json:"interval"`
		Failures int `json:"failures"`
		Seconds  int `json:"seconds"`
	} `json:"backoff"`
	FireKeys []struct {
		UnixMicro int64  `json:"unix_micro"`
		Key       string `json:"key"`
	} `json:"fire_keys"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/backoff_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBackoffMatchesVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.Backoff) < 50 {
		t.Fatalf("vectors missing: %d", len(v.Backoff))
	}
	for _, c := range v.Backoff {
		if got := Backoff(c.Interval, c.Failures); got != c.Seconds {
			t.Errorf("Backoff(%d,%d)=%d want %d", c.Interval, c.Failures, got, c.Seconds)
		}
	}
	// a huge failure count must not overflow
	if got := Backoff(900, 5000); got != 86400 {
		t.Errorf("overflow guard: %d", got)
	}
}

func TestFireKeyMatchesPythonIsoformat(t *testing.T) {
	for _, c := range loadVectors(t).FireKeys {
		ts := time.UnixMicro(c.UnixMicro).UTC()
		if got := FireKey("ws-1", ts); got != c.Key {
			t.Errorf("FireKey=%q want %q", got, c.Key)
		}
	}
}

func panicClient(outreachcfg.IMAP) imapclient.Client { panic("client must not be built") }

func TestFeatureFlagOff(t *testing.T) {
	for _, tc := range []struct{ auto, poll bool }{{false, true}, {true, false}, {false, false}} {
		h := &Handler{Automations: tc.auto, Outreach: config.OutreachSettings{InboundPollEnabled: tc.poll}, NewClient: panicClient}
		c := &queue.Claim{WorkspaceID: "ws", Payload: json.RawMessage(`{"workspace_id":"ws"}`)}
		if err := h.Handle(context.Background(), c); err != nil {
			t.Fatalf("auto=%v poll=%v: %v", tc.auto, tc.poll, err)
		}
	}
}

func TestMissingWorkspaceIDReturnsWithoutEffect(t *testing.T) {
	h := &Handler{Automations: true, Outreach: config.OutreachSettings{InboundPollEnabled: true}, NewClient: panicClient}
	for _, p := range []string{`{}`, `{"workspace_id":""}`, `not json`} {
		if err := h.Handle(context.Background(), &queue.Claim{Payload: json.RawMessage(p)}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

func TestIsConnErrOnlyTransportErrors(t *testing.T) {
	yes := []error{io.EOF, fmt.Errorf("wrapped: %w", io.EOF), net.ErrClosed, &net.OpError{Op: "read", Err: errors.New("reset")}, imapclient.ErrBudgetExceeded,
		fmt.Errorf("fetch: %w", imapclient.ErrBudgetExceeded)}
	for _, e := range yes {
		if !isConnErr(e) {
			t.Errorf("%v should be a connection error", e)
		}
	}
	no := []error{errors.New("mailbox closed"), errors.New("connection reset by peer text"), errors.New("not connected"),
		&imap.Error{Type: imap.StatusResponseTypeNo, Text: "mailbox closed"}, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "broken pipe"},
		imapclient.ErrOversized}
	for _, e := range no {
		if isConnErr(e) {
			t.Errorf("%v must not be a connection error", e)
		}
	}
}
