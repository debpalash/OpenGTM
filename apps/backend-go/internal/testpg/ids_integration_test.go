//go:build integration

package testpg

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Blocks handed out by concurrent ReserveIDs calls never overlap each other, never overlap ids that concurrent
// default-id inserts take from the sequence, and explicit-id inserts into them never collide.
func TestReserveIDsBlocksAreDisjointUnderConcurrency(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tbl := fmt.Sprintf("testpg_reserve_probe_%d", os.Getpid())
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl+`; CREATE TABLE `+tbl+` (id serial PRIMARY KEY, src text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+tbl) })
	// A row with an id the sequence has not reached: the first block must still start past it.
	if _, err := pool.Exec(ctx, `INSERT INTO `+tbl+` (id, src) VALUES (5000, 'explicit')`); err != nil {
		t.Fatal(err)
	}
	const reservers, perBlock, defaults = 8, 25, 400
	var wg sync.WaitGroup
	errs := make(chan error, reservers+defaults)
	for g := 0; g < reservers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			base := ReserveIDs(t, tbl, perBlock)
			if base < 5000 {
				errs <- fmt.Errorf("block base %d below existing max id 5000", base)
				return
			}
			for k := int64(1); k <= perBlock; k++ {
				if _, err := pool.Exec(ctx, `INSERT INTO `+tbl+` (id, src) VALUES ($1, 'block')`, base+k); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < defaults/4; i++ {
				if _, err := pool.Exec(ctx, `INSERT INTO `+tbl+` (src) VALUES ('default')`); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := 1 + reservers*perBlock + defaults; n != want {
		t.Fatalf("rows %d, want %d", n, want)
	}
	// After the reservations the sequence is past every row: a default insert succeeds.
	if _, err := pool.Exec(ctx, `INSERT INTO `+tbl+` (src) VALUES ('after')`); err != nil {
		t.Fatal(err)
	}
}
