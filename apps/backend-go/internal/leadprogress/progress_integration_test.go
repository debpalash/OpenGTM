//go:build integration

package leadprogress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadevents"
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

func cleanup(t *testing.T, c *redis.Client, ws ...string) {
	t.Cleanup(func() {
		for _, w := range ws {
			c.Del(context.Background(), RecentKey(w))
		}
	})
}

func TestEmitPublishesAndTrimsTo500(t *testing.T) {
	c := redisClient(t)
	ctx := context.Background()
	ws := uniq(t, "a")
	cleanup(t, c, ws)
	ps := c.Subscribe(ctx, Channel(ws))
	defer ps.Close()
	if _, err := ps.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	b := NewBus(c)
	b.EmitOrdered(ctx, ws, "first", Obj{{"i", int64(0)}})
	msg, err := ps.ReceiveMessage(ctx)
	if err != nil || msg.Payload == "" {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"type":"first","ts":%s,"i":0,"workspace_id":%q}`, strings.SplitN(strings.SplitN(msg.Payload, `"ts":`, 2)[1], ",", 2)[0], ws)
	if msg.Payload != want {
		t.Fatalf("payload %s\nwant %s", msg.Payload, want)
	}
	for i := 1; i < 505; i++ {
		b.EmitOrdered(ctx, ws, "t", Obj{{"i", int64(i)}})
	}
	n, _ := c.LLen(ctx, RecentKey(ws)).Result()
	if n != 500 {
		t.Fatalf("list len %d", n)
	}
	head, _ := c.LIndex(ctx, RecentKey(ws), 0).Result()
	tail, _ := c.LIndex(ctx, RecentKey(ws), -1).Result()
	if !strings.Contains(head, `"i":504`) || !strings.Contains(tail, `"i":5,`) {
		t.Fatalf("head %s tail %s", head, tail)
	}
}

func TestEmitUnscopedPublishesNothing(t *testing.T) {
	c := redisClient(t)
	ctx := context.Background()
	b := NewBus(c)
	ps := c.PSubscribe(ctx, "leadgen:progress:*")
	defer ps.Close()
	if _, err := ps.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	b.Emit(ctx, "", "x", map[string]any{"marker": uniq(t, "m")})
	rc, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if m, err := ps.ReceiveMessage(rc); err == nil && strings.Contains(m.Payload, "TestEmitUnscopedPublishesNothing") {
		t.Fatalf("unscoped event published: %s", m.Payload)
	}
}

func TestRecentOrderOldestFirst(t *testing.T) {
	c := redisClient(t)
	ctx := context.Background()
	ws := uniq(t, "a")
	cleanup(t, c, ws)
	b := NewBus(c)
	for i := 0; i < 5; i++ {
		b.Emit(ctx, ws, "t", map[string]any{"i": int64(i)})
	}
	rows, err := b.Recent(ctx, ws, 3)
	if err != nil || len(rows) != 3 {
		t.Fatalf("%v %v", rows, err)
	}
	for k, want := range []json.Number{"2", "3", "4"} {
		if rows[k]["i"] != want {
			t.Fatalf("row %d = %v", k, rows[k])
		}
	}
}

func TestRecentTenantKeyIsolation(t *testing.T) {
	c := redisClient(t)
	ctx := context.Background()
	a, other := uniq(t, "a"), uniq(t, "b")
	cleanup(t, c, a, other)
	NewBus(c).Emit(ctx, a, "secret", map[string]any{"message": "a-only"})
	// a fresh bus has no local deque, so only Redis could leak
	rows, _ := NewBus(c).Recent(ctx, other, 50)
	if len(rows) != 0 {
		t.Fatalf("cross-tenant read: %v", rows)
	}
	if rows, _ := NewBus(c).Recent(ctx, "", 50); len(rows) != 0 {
		t.Fatalf("empty tenant read: %v", rows)
	}
}

func norm(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecentMatchesPythonVectors(t *testing.T) {
	c := redisClient(t)
	ctx := context.Background()
	for _, v := range loadVectors(t).Recent {
		t.Run(v.Name, func(t *testing.T) {
			ws := uniq(t, "w")
			cleanup(t, c, ws)
			var b *Bus
			if v.RedisDown {
				b = NewBus(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond}))
			} else {
				b = NewBus(c)
				if len(v.List) > 0 {
					args := make([]any, len(v.List))
					for i, s := range v.List {
						args[i] = s
					}
					c.RPush(ctx, RecentKey(ws), args...) // list is newest first
				}
			}
			for _, ev := range v.Local {
				m := map[string]any{}
				for k, x := range ev {
					m[k] = x
				}
				if m["workspace_id"] == v.Workspace {
					m["workspace_id"] = ws
				}
				b.local = append(b.local, m)
			}
			got, err := b.Recent(ctx, ws, v.Limit)
			if err != nil {
				t.Fatal(err)
			}
			var want []any
			for _, r := range v.Result {
				var x any
				d := json.NewDecoder(bytes.NewReader(r))
				d.UseNumber()
				d.Decode(&x)
				if _, isObj := x.(map[string]any); !isObj {
					continue // documented deviation: non-object entries dropped
				}
				if m := x.(map[string]any); m["workspace_id"] == v.Workspace {
					m["workspace_id"] = ws
				}
				want = append(want, x)
			}
			if want == nil {
				want = []any{}
			}
			if g := norm(t, got); !reflect.DeepEqual(g, any(want)) {
				t.Fatalf("got %v\nwant %v", g, want)
			}
		})
	}
}

func TestRecentInvalidEntryFallsBackAndBacksOff(t *testing.T) {
	c := redisClient(t)
	ctx := context.Background()
	ws := uniq(t, "a")
	cleanup(t, c, ws)
	now := time.Unix(1_788_000_000, 0)
	b := NewBus(c)
	b.now = func() time.Time { return now }
	b.local = append(b.local, map[string]any{"type": "local", "workspace_id": ws})
	c.RPush(ctx, RecentKey(ws), `{"type":"ok","workspace_id":"`+ws+`"}`, "not json")
	rows, _ := b.Recent(ctx, ws, 10)
	if len(rows) != 1 || rows[0]["type"] != "local" {
		t.Fatalf("invalid entry must fall back to local like Python: %v", rows)
	}
	c.Del(ctx, RecentKey(ws))
	c.RPush(ctx, RecentKey(ws), `{"type":"ok","workspace_id":"`+ws+`"}`)
	if rows, _ = b.Recent(ctx, ws, 10); rows[0]["type"] != "local" {
		t.Fatalf("within 1s backoff Redis is skipped: %v", rows)
	}
	now = now.Add(1100 * time.Millisecond)
	if rows, _ = b.Recent(ctx, ws, 10); rows[0]["type"] != "ok" {
		t.Fatalf("after backoff Redis is read: %v", rows)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestEmittedEventReadableByPlan05SSERelay(t *testing.T) {
	c := redisClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	ws := uniq(t, "a")
	cleanup(t, c, ws)
	buf := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- leadevents.NewRelay(c, time.Hour).Stream(ctx, ws, buf, func() {}) }()
	defer func() { cancel(); <-done }()
	for i := 0; i < 200; i++ {
		if m, _ := c.PubSubNumSub(ctx, leadevents.Channel(ws)).Result(); m[leadevents.Channel(ws)] == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	NewBus(c).EmitOrdered(ctx, ws, "stage_start", Obj{{"job_id", "j1"}, {"message", "café"}})
	for i := 0; i < 200 && !strings.Contains(buf.String(), "stage_start"); i++ {
		time.Sleep(25 * time.Millisecond)
	}
	got := buf.String()
	if !strings.HasPrefix(got, `data: {"type": "stage_start", "ts": `) ||
		!strings.HasSuffix(got, fmt.Sprintf(`, "job_id": "j1", "message": "café", "workspace_id": %q}`+"\n\n", ws)) {
		t.Fatalf("relay frame %q", got)
	}
}
