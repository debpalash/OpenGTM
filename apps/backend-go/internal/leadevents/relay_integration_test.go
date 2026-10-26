//go:build integration

package leadevents

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opt)
	t.Cleanup(func() { c.Close() })
	return c
}

func uniq(t *testing.T, s string) string {
	return fmt.Sprintf("%s-%d-%s", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano(), s)
}

func numsub(t *testing.T, c *redis.Client, ch string) int64 {
	t.Helper()
	m, err := c.PubSubNumSub(context.Background(), ch).Result()
	if err != nil {
		t.Fatal(err)
	}
	return m[ch]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func start(t *testing.T, r *Relay, ws string) (*syncBuf, context.CancelFunc, chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	buf := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- r.Stream(ctx, ws, buf, func() {}) }()
	return buf, cancel, done
}

func TestStreamDeliversOnlyOwnWorkspace(t *testing.T) {
	c := redisClient(t)
	a, b := uniq(t, "a"), uniq(t, "b")
	buf, cancel, done := start(t, NewRelay(c, time.Hour), a)
	defer func() { cancel(); <-done }()
	waitFor(t, "subscribe", func() bool { return numsub(t, c, Channel(a)) == 1 })
	ctx := context.Background()
	c.Publish(ctx, Channel(b), `{"type":"progress","who":"b"}`)
	c.Publish(ctx, Channel(a), `{"type":"progress","who":"a","pct":1.0}`)
	waitFor(t, "frame", func() bool { return strings.Contains(buf.String(), `"who": "a"`) })
	if strings.Contains(buf.String(), `"b"`) {
		t.Fatalf("leaked: %q", buf.String())
	}
	if buf.String() != "data: {\"type\": \"progress\", \"who\": \"a\", \"pct\": 1.0}\n\n" {
		t.Fatalf("bytes %q", buf.String())
	}
}

func TestStreamClosesSubscriptionOnCancel(t *testing.T) {
	c := redisClient(t)
	ws := uniq(t, "w")
	_, cancel, done := start(t, NewRelay(c, time.Hour), ws)
	waitFor(t, "subscribe", func() bool { return numsub(t, c, Channel(ws)) == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "unsubscribe", func() bool { return numsub(t, c, Channel(ws)) == 0 })
}

func TestStreamSkipsMalformedPayload(t *testing.T) {
	c := redisClient(t)
	ws := uniq(t, "w")
	buf, cancel, done := start(t, NewRelay(c, time.Hour), ws)
	defer func() { cancel(); <-done }()
	waitFor(t, "subscribe", func() bool { return numsub(t, c, Channel(ws)) == 1 })
	ctx := context.Background()
	c.Publish(ctx, Channel(ws), "not json")
	c.Publish(ctx, Channel(ws), `{"type":"ok"}`)
	waitFor(t, "frame", func() bool { return strings.Contains(buf.String(), "ok") })
	if buf.String() != "data: {\"type\": \"ok\"}\n\n" {
		t.Fatalf("bytes %q", buf.String())
	}
}

// Kills only this test's pub/sub connection (by client ID), never the shared
// Redis' other connections.
func TestStreamSurvivesRedisRestart(t *testing.T) {
	c := redisClient(t)
	ws := uniq(t, "w")
	name := uniq(t, "client")
	opt := c.Options()
	cp := *opt
	cp.ClientName = name
	rc := redis.NewClient(&cp)
	defer rc.Close()
	buf, cancel, done := start(t, NewRelay(rc, 50*time.Millisecond), ws)
	defer func() { cancel(); <-done }()
	waitFor(t, "subscribe", func() bool { return numsub(t, c, Channel(ws)) == 1 })
	ctx := context.Background()
	list, err := c.Do(ctx, "CLIENT", "LIST", "TYPE", "pubsub").Text()
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, line := range strings.Split(list, "\n") {
		if strings.Contains(line, "name="+name+" ") {
			for _, f := range strings.Fields(line) {
				if strings.HasPrefix(f, "id=") {
					id = strings.TrimPrefix(f, "id=")
				}
			}
		}
	}
	if id == "" {
		t.Fatalf("pubsub client %q not found", name)
	}
	if err := c.Do(ctx, "CLIENT", "KILL", "ID", id).Err(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resubscribe", func() bool { return numsub(t, c, Channel(ws)) == 1 })
	c.Publish(ctx, Channel(ws), `{"type":"after"}`)
	waitFor(t, "frame after restart", func() bool { return strings.Contains(buf.String(), `"after"`) })
	select {
	case err := <-done:
		t.Fatalf("stream ended: %v", err)
	default:
	}
}

// proxy forwards to target until closed; restart() reopens on the same address.
type proxy struct {
	addr, target string
	mu           sync.Mutex
	ln           net.Listener
	conns        []net.Conn
}

func (p *proxy) open(t *testing.T) {
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.ln, p.addr = ln, ln.Addr().String()
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", p.target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, u)
			p.mu.Unlock()
			go func() { io.Copy(u, c); u.Close(); c.Close() }()
			go func() { io.Copy(c, u); u.Close(); c.Close() }()
		}
	}()
}

func (p *proxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ln.Close()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

// Redis unreachable at start and later again: heartbeats keep flowing through
// the outage and events arrive after recovery (custom backoff reconnect path).
func TestStreamReconnectsAfterOutage(t *testing.T) {
	c := redisClient(t)
	opt := c.Options()
	p := &proxy{addr: "127.0.0.1:0", target: opt.Addr}
	p.open(t)
	addr := p.addr
	p.stop() // outage before the first subscribe
	cp := *opt
	cp.Addr = addr
	cp.MaxRetries = -1
	rc := redis.NewClient(&cp)
	defer rc.Close()
	r := NewRelay(rc, 30*time.Millisecond)
	r.minBack, r.maxBack = 40*time.Millisecond, 80*time.Millisecond
	ws := uniq(t, "w")
	buf, cancel, done := start(t, r, ws)
	defer func() { cancel(); <-done; p.stop() }()
	hbs := func() int { return strings.Count(buf.String(), string(HeartbeatFrame())) }
	waitFor(t, "heartbeats during outage", func() bool { return hbs() >= 3 })
	p.addr = addr
	p.open(t) // Redis recovers
	waitFor(t, "subscribe after recovery", func() bool { return numsub(t, c, Channel(ws)) == 1 })
	c.Publish(context.Background(), Channel(ws), `{"type":"back"}`)
	waitFor(t, "event after recovery", func() bool { return strings.Contains(buf.String(), `"back"`) })
	// second outage: kill the proxy, heartbeats continue, then recover again
	p.stop()
	before := hbs()
	waitFor(t, "heartbeats in second outage", func() bool { return hbs() >= before+2 })
	p.open(t)
	waitFor(t, "resubscribe", func() bool { return numsub(t, c, Channel(ws)) == 1 })
	c.Publish(context.Background(), Channel(ws), `{"type":"again"}`)
	waitFor(t, "event after 2nd recovery", func() bool { return strings.Contains(buf.String(), `"again"`) })
}

func TestStreamRejectsEmptyWorkspace(t *testing.T) {
	if err := NewRelay(nil, time.Hour).Stream(context.Background(), "", io.Discard, nil); err == nil {
		t.Fatal("expected error")
	}
}
