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
			if base < SeqFloor {
				errs <- fmt.Errorf("block base %d below SeqFloor", base)
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

// NewAppPool raises every public sequence to SeqFloor and never lowers one that is already past it.
func TestNewAppPoolRaisesSequencesToFloor(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	low, high := fmt.Sprintf("testpg_floor_low_%d", os.Getpid()), fmt.Sprintf("testpg_floor_high_%d", os.Getpid())
	if _, err := owner.Exec(ctx, `DROP SEQUENCE IF EXISTS `+low+`, `+high+`; CREATE SEQUENCE `+low+`; CREATE SEQUENCE `+high+`;
		SELECT setval('`+high+`', `+fmt.Sprint(5*SeqFloor)+`)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DROP SEQUENCE IF EXISTS `+low+`, `+high) })
	_ = NewAppPool(t, 1)
	var l, h int64
	if err := owner.QueryRow(ctx, `SELECT nextval('`+low+`'), nextval('`+high+`')`).Scan(&l, &h); err != nil {
		t.Fatal(err)
	}
	if l != SeqFloor+1 || h != 5*SeqFloor+1 {
		t.Fatalf("nextval low=%d high=%d, want %d and %d", l, h, SeqFloor+1, 5*SeqFloor+1)
	}
}
