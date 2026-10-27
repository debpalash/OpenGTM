// Command suppressnorm is a one-shot maintenance job: it rewrites legacy Gmail suppression rows (stored dotted or
// with a +tag by the Python stack) to the collapsed key Go matches and writes. It is idempotent, works in
// batches (one short transaction each) and is safe to re-run or interrupt.
//
//	DATABASE_URL=... suppressnorm [--batch 500] [--workspace ID] [--dry-run]
//
// A legacy row whose collapsed key already exists is merged into it (a lock is never lost) and removed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Stats counts what a run did (or, with DryRun, would do).
type Stats struct {
	Workspaces int
	Scanned    int
	Rekeyed    int
	Merged     int
}

// Options configures a run.
type Options struct {
	BatchSize int
	Workspace string // "" = every workspace
	DryRun    bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "suppressnorm:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("suppressnorm", flag.ContinueOnError)
	var o Options
	fs.IntVar(&o.BatchSize, "batch", 500, "rows per transaction")
	fs.StringVar(&o.Workspace, "workspace", "", "only this workspace id")
	fs.BoolVar(&o.DryRun, "dry-run", false, "count the rewrites without applying them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return errors.New("cannot open the database pool")
	}
	defer pool.Close()
	st, err := Normalize(ctx, pool, o)
	fmt.Fprintf(out, "workspaces=%d scanned=%d rekeyed=%d merged=%d dry_run=%t\n", st.Workspaces, st.Scanned, st.Rekeyed, st.Merged, o.DryRun)
	return err
}

// Normalize rewrites the legacy Gmail suppression rows of every (or one) workspace.
func Normalize(ctx context.Context, pool *pgxpool.Pool, o Options) (Stats, error) {
	var st Stats
	if o.BatchSize < 1 {
		o.BatchSize = 500
	}
	var wss []string
	if o.Workspace != "" {
		wss = []string{o.Workspace}
	} else {
		rows, err := pool.Query(ctx, `SELECT id FROM workspaces ORDER BY id`)
		if err != nil {
			return st, fmt.Errorf("list workspaces: %w", err)
		}
		wss, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return st, fmt.Errorf("list workspaces: %w", err)
		}
	}
	for _, ws := range wss {
		st.Workspaces++
		last := int32(0)
		for {
			n, next, err := batch(ctx, pool, ws, last, o, &st)
			if err != nil {
				return st, fmt.Errorf("workspace %s: %w", ws, err)
			}
			if n < o.BatchSize {
				break
			}
			last = next
		}
	}
	return st, nil
}

type row struct {
	id                    int32
	email, reason, source string
	locked                bool
}

// batch handles one page of gmail rows after id `last` and returns how many it read and the last id seen.
func batch(ctx context.Context, pool *pgxpool.Pool, ws string, last int32, o Options, st *Stats) (int, int32, error) {
	read, next := 0, last
	var rekeyed, merged int
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		rekeyed, merged = 0, 0
		rs, err := tx.Query(ctx, `SELECT id, email, COALESCE(reason,''), COALESCE(source,''), COALESCE(locked,false)
			FROM outreach_suppressions
			WHERE workspace_id = $1 AND id > $2 AND (lower(email) LIKE '%@gmail.com' OR lower(email) LIKE '%@googlemail.com')
			ORDER BY id LIMIT $3`, ws, last, o.BatchSize)
		if err != nil {
			return err
		}
		page, err := pgx.CollectRows(rs, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.id, &x.email, &x.reason, &x.source, &x.locked)
		})
		if err != nil {
			return err
		}
		read = len(page)
		for _, r := range page {
			next = r.id
			key := emailnorm.StoreKey(r.email)
			if key == "" || key == r.email {
				continue
			}
			var target row
			err := tx.QueryRow(ctx, `SELECT id, email, COALESCE(reason,''), COALESCE(source,''), COALESCE(locked,false)
				FROM outreach_suppressions WHERE workspace_id = $1 AND email = $2`, ws, key).
				Scan(&target.id, &target.email, &target.reason, &target.source, &target.locked)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				rekeyed++
				if !o.DryRun {
					if _, err := tx.Exec(ctx, `UPDATE outreach_suppressions SET email = $1 WHERE workspace_id = $2 AND id = $3`, key, ws, r.id); err != nil {
						return err
					}
				}
			case err != nil:
				return err
			default:
				merged++
				if o.DryRun {
					continue
				}
				if r.locked && !target.locked { // an opt-out is never downgraded by the merge
					if _, err := tx.Exec(ctx, `UPDATE outreach_suppressions SET locked = true, reason = $1, source = $2 WHERE workspace_id = $3 AND id = $4`,
						r.reason, r.source, ws, target.id); err != nil {
						return err
					}
				}
				if _, err := tx.Exec(ctx, `DELETE FROM outreach_suppressions WHERE workspace_id = $1 AND id = $2`, ws, r.id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, last, err
	}
	st.Scanned += read
	st.Rekeyed += rekeyed
	st.Merged += merged
	return read, next, nil
}

var testHookBeforeRekey func(key string)
