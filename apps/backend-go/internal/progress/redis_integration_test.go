//go:build integration

package progress_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
)

func TestPublishReachesSubscriber(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opt, _ := redis.ParseURL(url)
	sub := redis.NewClient(opt)
	defer sub.Close()
	ctx := context.Background()
	ps := sub.Subscribe(ctx, progress.Channel("wb-1"))
	defer ps.Close()
	if _, err := ps.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	pub, err := progress.NewRedis(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	// Publishing with an already-cancelled ctx must still deliver.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	pub.Publish(cctx, "wb-1", map[string]any{"type": "workbook_status", "status": "paused", "completed": 1, "errors": 0, "total": 3})
	select {
	case m := <-ps.Channel():
		var got map[string]any
		if err := json.Unmarshal([]byte(m.Payload), &got); err != nil {
			t.Fatal(err)
		}
		if got["type"] != "workbook_status" || got["status"] != "paused" || got["total"].(float64) != 3 {
			t.Fatalf("unexpected message %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message")
	}
}

func TestPublishFailureIsSwallowed(t *testing.T) {
	pub, err := progress.NewRedis("redis://127.0.0.1:1/0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	pub.Publish(context.Background(), "wb", map[string]any{"type": "x"}) // must not panic or block long
}
