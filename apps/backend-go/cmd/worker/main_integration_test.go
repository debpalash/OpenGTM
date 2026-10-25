//go:build integration

package main

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// With MaxActivePerWorkspace=1 and two slots, two jobs of one workspace must
// never run concurrently.
func TestSlotLoopHonoursTenantCap(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		  VALUES ('run_workbook','{}','pending',0,now() AT TIME ZONE 'UTC',0,3,'ws','go')`); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{MaxActivePerWorkspace: 1, HeartbeatInterval: time.Hour}
	var running, maxRunning, total atomic.Int32
	rn := func(ctx context.Context, _ *queue.Claim) error {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(700 * time.Millisecond)
		running.Add(-1)
		total.Add(1)
		return nil
	}
	cctx, stop := context.WithTimeout(ctx, 6*time.Second)
	defer stop()
	log := telemetry.NewLogger(io.Discard)
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() { slotLoop(cctx, ctx, pool, cfg, log, "w", rn); done <- struct{}{} }()
	}
	for total.Load() < 2 && cctx.Err() == nil {
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	<-done
	<-done
	if total.Load() != 2 {
		t.Fatalf("only %d jobs ran", total.Load())
	}
	if maxRunning.Load() != 1 {
		t.Fatalf("max concurrent = %d, want 1", maxRunning.Load())
	}
}
