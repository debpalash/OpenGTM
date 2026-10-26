package leadevents

import (
	"context"
	"io"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	minBackoff     = time.Second
	maxBackoff     = 5 * time.Second
	subscribeLimit = 2 * time.Second
)

// Relay forwards one workspace's Redis progress events to an SSE writer.
type Relay struct {
	c         *redis.Client
	heartbeat time.Duration
	minBack   time.Duration
	maxBack   time.Duration
}

// NewRelay builds a relay. A nil client yields heartbeats only.
func NewRelay(c *redis.Client, heartbeat time.Duration) *Relay {
	return &Relay{c: c, heartbeat: heartbeat, minBack: minBackoff, maxBack: maxBackoff}
}

// Stream subscribes to exactly Channel(workspaceID) and writes SSE frames until
// ctx is done (then returns nil) or a write fails. Malformed payloads are
// skipped. A heartbeat is sent after `heartbeat` of silence, including while
// Redis is unavailable; reconnects back off from 1 s to 5 s. The subscription
// is always closed on return.
func (r *Relay) Stream(ctx context.Context, workspaceID string, w io.Writer, flush func()) error {
	var (
		ps      *redis.PubSub
		msgs    <-chan *redis.Message
		retry   <-chan time.Time
		backoff = r.minBack
	)
	defer func() {
		if ps != nil {
			_ = ps.Close()
		}
	}()
	write := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
		return nil
	}
	drop := func() {
		if ps != nil {
			_ = ps.Close()
			ps, msgs = nil, nil
		}
		t := time.After(backoff)
		retry = t
		if backoff *= 2; backoff > r.maxBack {
			backoff = r.maxBack
		}
	}
	connect := func() {
		retry = nil
		if r.c == nil {
			return
		}
		p := r.c.Subscribe(ctx, Channel(workspaceID))
		cctx, cancel := context.WithTimeout(ctx, subscribeLimit)
		_, err := p.Receive(cctx)
		cancel()
		if err != nil {
			_ = p.Close()
			if ctx.Err() == nil {
				drop()
			}
			return
		}
		ps, msgs, backoff = p, p.Channel(), r.minBack
	}
	connect()

	hb := time.NewTimer(r.heartbeat)
	defer hb.Stop()
	resetHB := func() {
		if !hb.Stop() {
			select {
			case <-hb.C:
			default:
			}
		}
		hb.Reset(r.heartbeat)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-hb.C:
			if err := write(HeartbeatFrame()); err != nil {
				return err
			}
			hb.Reset(r.heartbeat)
		case <-retry:
			connect()
		case m, ok := <-msgs:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				drop()
				continue
			}
			enc, err := EncodeEvent([]byte(m.Payload))
			if err != nil {
				continue
			}
			if err := write(Frame(enc)); err != nil {
				return err
			}
			resetHB()
		}
	}
}
