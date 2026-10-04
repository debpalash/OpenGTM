package progress

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
)

func TestEncodeBoundsPayload(t *testing.T) {
	at := time.Unix(0, 0)
	small, err := Encode("ws", "row.updated", map[string]int{"n": 1}, at)
	if err != nil {
		t.Fatal(err)
	}
	var e Event
	json.Unmarshal(small, &e)
	if e.WorkspaceID != "ws" || e.Event != "row.updated" || string(e.Data) != `{"n":1}` || e.Truncated {
		t.Fatalf("small event = %+v", e)
	}

	big, err := Encode("ws", "row.updated", map[string]string{"blob": strings.Repeat("x", 10000)}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(big) > MaxPayload {
		t.Fatalf("payload %d bytes exceeds %d", len(big), MaxPayload)
	}
	e = Event{}
	json.Unmarshal(big, &e)
	if !e.Truncated || e.Data != nil {
		t.Fatalf("oversized data must be dropped and flagged: %+v", e)
	}

	for _, bad := range [][2]string{{"", "e"}, {"ws", ""}, {"ws", strings.Repeat("e", 9000)}} {
		if _, err := Encode(bad[0], bad[1], nil, at); err == nil {
			t.Errorf("Encode(%q, %d-byte event) accepted", bad[0], len(bad[1]))
		}
	}
}

func TestSubscriptionsFilterByWorkspaceAndCloseCleanly(t *testing.T) {
	h := NewHub(nil, slog.New(slog.DiscardHandler))
	a := h.Subscribe("ws-a", 1)
	b := h.Subscribe("ws-b", 1)
	h.dispatch(Event{WorkspaceID: "ws-a", Event: "x"})
	h.dispatch(Event{WorkspaceID: "ws-a", Event: "overflow"})
	if e := <-a.C(); e.Event != "x" {
		t.Fatalf("a got %+v", e)
	}
	if a.Dropped() != 1 {
		t.Fatalf("slow subscriber should drop, dropped=%d", a.Dropped())
	}
	select {
	case e := <-b.C():
		t.Fatalf("ws-b received ws-a event %+v", e)
	default:
	}
	a.Close()
	a.Close()
	if _, ok := <-a.C(); ok {
		t.Fatal("closed subscription channel still open")
	}
	h.dispatch(Event{WorkspaceID: "ws-a", Event: "after-close"}) // must not panic
	b.Close()
}

func TestPublishDeliversOnlyCommittedEvents(t *testing.T) {
	owner := dbtest.OwnerURL(t)
	pool := dbtest.Pool(t, dbtest.RoleURL(t, owner, "opengtm_core_progress_test", "progress_test_only"), 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub(pool, slog.New(slog.DiscardHandler))
	go hub.Run(ctx)
	select {
	case <-hub.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not start")
	}
	subA := hub.Subscribe("ws-a", 16)
	subB := hub.Subscribe("ws-b", 16)
	defer subA.Close()
	defer subB.Close()

	publish := func(ws, event string, commit bool) {
		t.Helper()
		err := db.WithoutTenant(ctx, pool, func(tx pgx.Tx) error {
			if err := Publish(ctx, tx, ws, event, map[string]string{"k": event}); err != nil {
				return err
			}
			if !commit {
				return errRollback
			}
			return nil
		})
		if commit && err != nil {
			t.Fatal(err)
		}
	}
	publish("ws-a", "rolled-back", false)
	publish("ws-b", "for-b", true)
	publish("ws-a", "committed", true)

	select {
	case e := <-subA.C():
		if e.Event != "committed" || string(e.Data) != `{"k":"committed"}` {
			t.Fatalf("ws-a got %+v (a rolled-back event must never arrive)", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("committed event not delivered")
	}
	select {
	case e := <-subB.C():
		if e.Event != "for-b" {
			t.Fatalf("ws-b got %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ws-b event not delivered")
	}
	select {
	case e := <-subA.C():
		t.Fatalf("unexpected extra event for ws-a: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

var errRollback = errors.New("rollback")
