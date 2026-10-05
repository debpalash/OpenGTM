package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/lease"
)

func init() {
	register("leases", "list scheduler leases or release one whose holder is gone", runLeases)
}

const leasesUsage = `usage:
  opengtm leases list [--json] [--config FILE]
  opengtm leases release <name> [--config FILE]

Scheduler replicas elect a leader per periodic scheduler through the
scheduler_leases table (enable with SCHEDULER_LEADER_ELECTION=true on the
Python scheduler). "release" expires a lease now so a peer leads at once; use it
only when the holder is known to be gone, otherwise wait out the TTL. A holder
that is somehow still alive is fenced out by the higher token the next leader
gets.`

func runLeases(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(leasesUsage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("leases "+sub, flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to opengtm.yaml (default $"+config.ConfigEnv+")")
	asJSON := fs.Bool("json", false, "print JSON (list)")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL, db.Options{AppName: "opengtm-leases", MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()

	switch sub {
	case "list":
		if len(pos) != 0 {
			return errors.New(leasesUsage)
		}
		rows, err := lease.Snapshot(ctx, pool)
		if err != nil {
			return err
		}
		if *asJSON {
			if rows == nil {
				rows = []lease.Row{}
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "LEASE\tHOLDER\tTOKEN\tSTATE\tEXPIRES")
		for _, r := range rows {
			state := "expired"
			if r.Live {
				state = "live"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", r.Name, r.Holder, r.Token, state, r.ExpiresAt.UTC().Format(time.RFC3339))
		}
		return tw.Flush()
	case "release":
		if len(pos) != 1 {
			return errors.New(leasesUsage)
		}
		ok, err := lease.ForceRelease(ctx, pool, pos[0])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no lease named %q", pos[0])
		}
		fmt.Printf("%s released; the next replica to poll leads\n", pos[0])
		return nil
	default:
		return errors.New(leasesUsage)
	}
}
