//go:build integration

package autopilot

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	r := redis.NewClient(opt)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRedisPlanStoreSingleUse(t *testing.T) {
	ctx := context.Background()
	r := testRedis(t)
	prefix := "test:autopilot:" + strings.ReplaceAll(t.Name(), "/", "_") + ":"
	t.Cleanup(func() {
		keys, _ := r.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			r.Del(ctx, keys...)
		}
	})
	var ttl atomic.Int64
	ttl.Store(int64(time.Hour))
	s := &RedisPlanStore{R: r, Prefix: prefix, TTL: func(context.Context) time.Duration { return time.Duration(ttl.Load()) }}
	testPlanStore(t, ctx, s, func() { ttl.Store(int64(time.Second)) }, func() { time.Sleep(1500 * time.Millisecond) })

	// Concurrent approvals of one plan: exactly one consume wins.
	ttl.Store(int64(time.Hour))
	id, nonce, err := s.Put(ctx, "ws-1", "7", []byte(`{"goal": "race"}`))
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := s.Consume(ctx, "ws-1", "7", id, nonce); err == nil && p != nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d consumes won", wins.Load())
	}
}

func TestRedisMemory(t *testing.T) {
	ctx := context.Background()
	r := testRedis(t)
	prefix := "test:autopilot:mem:"
	t.Cleanup(func() { r.Del(ctx, prefix+"ws-m") })
	r.Del(ctx, prefix+"ws-m")
	clock := time.Unix(1000, 0)
	m := &RedisMemory{R: r, Prefix: prefix, Now: func() time.Time { clock = clock.Add(time.Second); return clock }}
	for i := 0; i < 55; i++ {
		wb := "wb-" + itoa(i%52)
		if err := m.Record(ctx, "ws-m", "goal "+itoa(i), &wb, "ok"); err != nil {
			t.Fatal(err)
		}
	}
	_ = m.Record(ctx, "ws-m", "no wb", nil, "")
	got, err := m.Recent(ctx, "ws-m", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxPerWorkspace || got[0].Goal != "no wb" || got[0].Outcome != "ok" || got[1].Goal != "goal 54" ||
		*got[1].WorkbookID != "wb-2" {
		t.Fatalf("%d rows, first %+v %+v", len(got), got[0], got[1])
	}
	if top, _ := m.Recent(ctx, "ws-m", 3); len(top) != 3 {
		t.Fatalf("limit: %d", len(top))
	}
}
