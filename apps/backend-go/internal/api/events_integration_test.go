//go:build integration

package api

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	u := os.Getenv("TEST_REDIS_URL")
	if u == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(u)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opt)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestEventsStreamsPublishedEvent(t *testing.T) {
	rc := testRedis(t)
	setHeartbeat(t, time.Hour)
	e := newEventsEnv(t, func(d *Deps) { d.Redis = rc })
	ws := e.seedWS(t, fmt.Sprintf("t16p%d", time.Now().UnixNano()%1e9))
	user := strings.TrimSuffix(ws, "-ws") + "-alice"
	tok := e.tokenQuery(t, user, []string{"pwd"})
	resp, _, _ := openStream(t, e, "/api/events?token="+tok+"&workspace_id="+ws, nil)
	ch := "leadgen:progress:" + ws
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, _ := rc.PubSubNumSub(context.Background(), ch).Result()
		if m[ch] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no subscription")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// other workspace's channel must not reach this client
	rc.Publish(context.Background(), "leadgen:progress:other", `{"type":"x"}`)
	rc.Publish(context.Background(), ch, `{"type":"lead","n":1.50,"name":"é<b>","a":{"z":1,"b":2}}`)
	line := make(chan string, 1)
	go func() { s, _ := bufio.NewReader(resp.Body).ReadString('\n'); line <- s }()
	select {
	case s := <-line:
		want := `data: {"type": "lead", "n": 1.5, "name": "é<b>", "a": {"z": 1, "b": 2}}` + "\n"
		if s != want {
			t.Fatalf("got %q want %q", s, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
	}
}
