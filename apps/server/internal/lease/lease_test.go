package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
)

// These run against the real Alembic schema as a NOSUPERUSER NOBYPASSRLS member
// of the runtime group. Every test uses its own lease names.

var (
	once     sync.Once
	appURL   string
	ownerURL string
)

func setup(t *testing.T) {
	t.Helper()
	owner := dbtest.OwnerURL(t)
	once.Do(func() {
		dbtest.Migrate(t, owner)
		ownerURL = owner
		appURL = dbtest.AppURL(t, owner)
		p := dbtest.Pool(t, owner, 2)
		for _, s := range []string{
			`CREATE TABLE IF NOT EXISTS lease_probe_go (id BIGSERIAL PRIMARY KEY, holder TEXT, token BIGINT)`,
			`GRANT SELECT, INSERT ON lease_probe_go TO yupcha_app`,
			`GRANT USAGE, SELECT ON SEQUENCE lease_probe_go_id_seq TO yupcha_app`,
		} {
			if _, err := p.Exec(context.Background(), s); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func newPool(t *testing.T, max int32) *pgxpool.Pool {
	t.Helper()
	setup(t)
	return dbtest.Pool(t, appURL, max)
}

func uniq(t *testing.T) string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "t-" + hex.EncodeToString(b[:])
}

func TestAcquireRenewReleaseTokens(t *testing.T) {
	pool := newPool(t, 4)
	name := uniq(t)
	a := New(pool, name, "A", 30*time.Second)
	b := New(pool, name, "B", 30*time.Second)
	ctx := context.Background()

	if ok, err := a.Acquire(ctx); err != nil || !ok {
		t.Fatalf("A acquire: %v %v", ok, err)
	}
	if tok, _ := a.Token(); tok != 1 {
		t.Fatalf("token = %d, want 1", tok)
	}
	if ok, _ := b.Acquire(ctx); ok {
		t.Fatal("B acquired a live lease")
	}
	if ok, _ := a.Acquire(ctx); !ok {
		t.Fatal("A renew failed")
	}
	if tok, _ := a.Token(); tok != 1 {
		t.Fatalf("a live renewal changed the token to %d", tok)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Held() {
		t.Fatal("still held after release")
	}
	if ok, _ := b.Acquire(ctx); !ok {
		t.Fatal("B could not take a released lease")
	}
	if tok, _ := b.Token(); tok != 2 {
		t.Fatalf("B token = %d, want 2 (never reset)", tok)
	}
}

func TestExpiryTakeoverDeposesTheOldHolder(t *testing.T) {
	pool := newPool(t, 4)
	name := uniq(t)
	a := New(pool, name, "A", 300*time.Millisecond)
	b := New(pool, name, "B", 30*time.Second)
	ctx := context.Background()
	if ok, _ := a.Acquire(ctx); !ok {
		t.Fatal("A acquire")
	}
	time.Sleep(500 * time.Millisecond)
	if a.Held() {
		t.Fatal("A should distrust its lease locally after the TTL")
	}
	if ok, _ := b.Acquire(ctx); !ok {
		t.Fatal("B should take the expired lease")
	}
	if tok, _ := b.Token(); tok != 2 {
		t.Fatalf("B token = %d", tok)
	}
	if ok, _ := a.Acquire(ctx); ok {
		t.Fatal("deposed A re-acquired a live lease")
	}
	if _, has := a.Token(); has {
		t.Fatal("A kept its stale token")
	}
}

func TestSameHolderAfterLapseGetsNewToken(t *testing.T) {
	pool := newPool(t, 2)
	a := New(pool, uniq(t), "A", 300*time.Millisecond)
	ctx := context.Background()
	_, _ = a.Acquire(ctx)
	time.Sleep(500 * time.Millisecond)
	if ok, _ := a.Acquire(ctx); !ok {
		t.Fatal("re-acquire")
	}
	if tok, _ := a.Token(); tok != 2 {
		t.Fatalf("token = %d, want 2", tok)
	}
}

func TestContentionExactlyOneLeader(t *testing.T) {
	setup(t)
	name := uniq(t)
	const n = 12
	leases := make([]*Lease, n)
	for i := range leases {
		leases[i] = New(dbtest.Pool(t, appURL, 1), name, fmt.Sprintf("H%d", i), 10*time.Second)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := map[string]int64{}
	start := make(chan struct{})
	for _, l := range leases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 20 {
				if ok, err := l.Acquire(context.Background()); err != nil {
					t.Error(err)
				} else if ok {
					tok, _ := l.Token()
					mu.Lock()
					wins[l.Holder] = tok
					mu.Unlock()
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(wins) != 1 {
		t.Fatalf("%d holders won: %v", len(wins), wins)
	}
	for _, tok := range wins {
		if tok != 1 {
			t.Fatalf("leader's token changed while live: %d", tok)
		}
	}
}

func TestWithFenceAbortsADeposedHoldersWrite(t *testing.T) {
	pool := newPool(t, 4)
	name := uniq(t)
	a := New(pool, name, "A", 300*time.Millisecond)
	b := New(pool, name, "B", 30*time.Second)
	ctx := context.Background()
	_, _ = a.Acquire(ctx)
	time.Sleep(500 * time.Millisecond)
	if ok, _ := b.Acquire(ctx); !ok {
		t.Fatal("B takeover")
	}
	// A is paused past its TTL and wakes up believing it still leads.
	a.mu.Lock()
	a.deadline = time.Now().Add(time.Hour)
	a.mu.Unlock()
	marker := "A-" + name
	err := a.WithFence(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO lease_probe_go (holder, token) VALUES ($1, 1)`, marker)
		return err
	})
	if !errors.Is(err, ErrLost) {
		t.Fatalf("err = %v, want ErrLost", err)
	}
	if err := b.WithFence(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO lease_probe_go (holder, token) VALUES ($1, 2)`, "B-"+name)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	rows, err := pool.Query(ctx, `SELECT holder FROM lease_probe_go WHERE holder LIKE '%' || $1`, name)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h string
		_ = rows.Scan(&h)
		got = append(got, h)
	}
	rows.Close()
	if len(got) != 1 || got[0] != "B-"+name {
		t.Fatalf("rows = %v, want only B's write", got)
	}
}

func TestTakeoverWaitsForAnInFlightFencedCommit(t *testing.T) {
	pool := newPool(t, 4)
	name := uniq(t)
	a := New(pool, name, "A", 800*time.Millisecond)
	b := New(pool, name, "B", 30*time.Second)
	ctx := context.Background()
	_, _ = a.Acquire(ctx)
	tok, _ := a.Token()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Fence(ctx, tx, tok); err != nil { // passes; row share-locked
		t.Fatal(err)
	}
	time.Sleep(time.Second) // expired, but A's transaction is still open
	done := make(chan bool, 1)
	go func() {
		ok, _ := b.Acquire(ctx)
		done <- ok
	}()
	select {
	case <-done:
		t.Fatal("takeover did not wait for the fenced transaction")
	case <-time.After(700 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("B should win after A's commit")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("takeover never completed")
	}
}

func TestRunKeepsLeadershipAndHandsOverOnCancel(t *testing.T) {
	pool := newPool(t, 4)
	name := uniq(t)
	a := New(pool, name, "A", time.Second)
	b := New(pool, name, "B", time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan bool, 8)
	finished := make(chan struct{})
	go func() {
		a.Run(ctx, 200*time.Millisecond, func(h bool) { changes <- h })
		close(finished)
	}()
	if !<-changes {
		t.Fatal("A should lead first")
	}
	deadline := time.Now().Add(2500 * time.Millisecond) // > 2 TTLs of heartbeats
	for time.Now().Before(deadline) {
		if ok, _ := b.Acquire(context.Background()); ok {
			t.Fatal("B took a heartbeated lease")
		}
		time.Sleep(150 * time.Millisecond)
	}
	if tok, _ := a.Token(); tok != 1 {
		t.Fatalf("token drifted to %d", tok)
	}
	cancel()
	<-finished
	if ok, _ := b.Acquire(context.Background()); !ok {
		t.Fatal("B should lead immediately after A's graceful shutdown")
	}
	if tok, _ := b.Token(); tok != 2 {
		t.Fatalf("B token = %d", tok)
	}
}

func TestSnapshotAndForceRelease(t *testing.T) {
	pool := newPool(t, 2)
	name := uniq(t)
	a := New(pool, name, "A", time.Minute)
	ctx := context.Background()
	_, _ = a.Acquire(ctx)
	rows, err := Snapshot(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var found *Row
	for i := range rows {
		if rows[i].Name == Prefix+name {
			found = &rows[i]
		}
	}
	if found == nil || found.Holder != "A" || !found.Live || found.Token != 1 {
		t.Fatalf("snapshot row = %+v", found)
	}
	if ok, err := ForceRelease(ctx, pool, name); err != nil || !ok {
		t.Fatalf("force release: %v %v", ok, err)
	}
	b := New(pool, name, "B", time.Minute)
	if ok, _ := b.Acquire(ctx); !ok {
		t.Fatal("B should lead after a forced release")
	}
	// A, if it were somehow alive, is fenced out by the higher token.
	err = a.WithFence(ctx, func(pgx.Tx) error { return nil })
	if !errors.Is(err, ErrLost) {
		t.Fatalf("stale A fence err = %v", err)
	}
}

// TestInteropFromPython is driven by tests/test_scheduler_lease_go_interop_pg.py
// to prove the Go and Python implementations share one protocol.
//
//	OPENGTM_INTEROP_PHASE=blocked   the lease is live and held by Python: Acquire must fail.
//	OPENGTM_INTEROP_PHASE=takeover  the lease has expired: Acquire must succeed; the token is printed.
func TestInteropFromPython(t *testing.T) {
	phase, name := os.Getenv("OPENGTM_INTEROP_PHASE"), os.Getenv("OPENGTM_INTEROP_LEASE")
	if phase == "" || name == "" {
		t.Skip("driven by the Python interop test")
	}
	pool := newPool(t, 2)
	l := New(pool, name, "go-holder", time.Minute)
	ok, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	switch phase {
	case "blocked":
		if ok {
			t.Fatal("Go acquired a lease Python holds")
		}
	case "takeover":
		if !ok {
			t.Fatal("Go could not take the expired Python lease")
		}
		tok, _ := l.Token()
		fmt.Printf("INTEROP_TOKEN=%d\n", tok)
	default:
		t.Fatalf("unknown phase %q", phase)
	}
}
