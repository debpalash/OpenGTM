//go:build integration

package leadevents

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// Killing the subscriber's connection (what a Redis restart does) must not end
// the stream; events published afterwards still arrive.
func TestStreamSurvivesRedisRestart(t *testing.T) {
	c := redisClient(t)
	ws := uniq(t, "w")
	r := NewRelay(c, 50*time.Millisecond)
	r.minBack, r.maxBack = 20*time.Millisecond, 50*time.Millisecond
	buf, cancel, done := start(t, r, ws)
	defer func() { cancel(); <-done }()
	waitFor(t, "subscribe", func() bool { return numsub(t, c, Channel(ws)) == 1 })
	ctx := context.Background()
	if err := c.Do(ctx, "CLIENT", "KILL", "TYPE", "pubsub").Err(); err != nil {
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
