// Package progress delivers workspace-scoped live events over PostgreSQL
// LISTEN/NOTIFY, the lite-profile replacement for the Redis progress channel.
//
// Events are published inside the writer's transaction, so subscribers only
// hear about work that actually committed. One dedicated connection per
// process LISTENs and fans events out in memory; each subscriber sees only
// its own workspace. NOTIFY is fire-and-forget: there is no replay, so
// clients should refetch state after (re)connecting.
package progress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel is the single NOTIFY channel shared by all workspaces; filtering
// happens in-process because channel names cannot be parameterised safely.
const Channel = "opengtm_progress"

// MaxPayload keeps well under PostgreSQL's 8000-byte NOTIFY limit.
const MaxPayload = 7900

// Event is the wire envelope.
type Event struct {
	WorkspaceID string          `json:"workspace_id"`
	Event       string          `json:"event"`
	Data        json.RawMessage `json:"data,omitempty"`
	// Truncated means Data exceeded the NOTIFY limit and was dropped; the
	// client should refetch the affected resource.
	Truncated bool      `json:"truncated,omitempty"`
	At        time.Time `json:"at"`
}

// Publish queues an event in tx; it is delivered only if tx commits.
func Publish(ctx context.Context, tx pgx.Tx, workspaceID, event string, data any) error {
	payload, err := Encode(workspaceID, event, data, time.Now())
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", Channel, string(payload)); err != nil {
		return fmt.Errorf("progress: notify: %w", err)
	}
	return nil
}

// Encode builds the bounded wire payload.
func Encode(workspaceID, event string, data any, at time.Time) ([]byte, error) {
	if workspaceID == "" {
		return nil, errors.New("progress: workspace id is required")
	}
	if event == "" {
		return nil, errors.New("progress: event name is required")
	}
	e := Event{WorkspaceID: workspaceID, Event: event, At: at.UTC()}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("progress: encode data: %w", err)
		}
		e.Data = raw
	}
	out, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(out) <= MaxPayload {
		return out, nil
	}
	e.Data, e.Truncated = nil, true
	out, err = json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxPayload {
		return nil, fmt.Errorf("progress: event %q envelope exceeds %d bytes", event, MaxPayload)
	}
	return out, nil
}

// Hub owns the LISTEN connection and in-process subscribers.
type Hub struct {
	pool  *pgxpool.Pool
	log   *slog.Logger
	ready chan struct{}
	once  sync.Once

	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewHub returns a hub; call Run to start listening.
func NewHub(pool *pgxpool.Pool, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{pool: pool, log: log.With("component", "progress"), ready: make(chan struct{}),
		subs: map[*Subscription]struct{}{}}
}

// Ready is closed once the first LISTEN is active.
func (h *Hub) Ready() <-chan struct{} { return h.ready }

// Run listens until ctx is cancelled, reconnecting with backoff. It uses its
// own connection rather than a pooled one so a long-lived LISTEN never
// starves request handlers of pool capacity.
func (h *Hub) Run(ctx context.Context) error {
	delay := 500 * time.Millisecond
	for {
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return nil
		}
		h.log.Warn("progress listener disconnected; reconnecting", "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, h.pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	h.once.Do(func() { close(h.ready) })
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var e Event
		if err := json.Unmarshal([]byte(n.Payload), &e); err != nil || e.WorkspaceID == "" {
			h.log.Warn("dropping malformed progress payload")
			continue
		}
		h.dispatch(e)
	}
}

func (h *Hub) dispatch(e Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if s.workspaceID != e.WorkspaceID {
			continue
		}
		select {
		case s.c <- e:
		default:
			// Never let one slow client stall delivery to everyone else.
			s.dropped.Add(1)
		}
	}
}

// Subscription receives one workspace's events.
type Subscription struct {
	hub         *Hub
	workspaceID string
	c           chan Event
	dropped     atomic.Int64
	closeOnce   sync.Once
}

// Subscribe registers interest in workspaceID's events.
func (h *Hub) Subscribe(workspaceID string, buffer int) *Subscription {
	if buffer <= 0 {
		buffer = 64
	}
	s := &Subscription{hub: h, workspaceID: workspaceID, c: make(chan Event, buffer)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// C delivers events; it is closed by Close.
func (s *Subscription) C() <-chan Event { return s.c }

// Dropped counts events discarded because the subscriber fell behind.
func (s *Subscription) Dropped() int64 { return s.dropped.Load() }

// Close unregisters the subscription.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.hub.mu.Lock()
		delete(s.hub.subs, s)
		s.hub.mu.Unlock()
		close(s.c)
	})
}
