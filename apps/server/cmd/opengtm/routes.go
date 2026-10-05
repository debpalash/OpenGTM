package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

func init() {
	register("routes", "list or change which executor (python|go) claims each job type", runRoutes)
}

const routesUsage = `usage:
  opengtm routes list [--json] [--config FILE]
  opengtm routes set <job_type> <python|go> [--force] [--config FILE]

Job types without a route are claimed by Python workers. "switchable" types have
a Go executor in this binary and are still Python-owned until set to go.`

func runRoutes(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(routesUsage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("routes "+sub, flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to opengtm.yaml (default $"+config.ConfigEnv+")")
	asJSON := fs.Bool("json", false, "print JSON (list)")
	force := fs.Bool("force", false, "switch even while jobs of the type are processing (set)")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL, db.Options{AppName: "opengtm-routes", MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()

	switch sub {
	case "list":
		if len(pos) != 0 {
			return errors.New(routesUsage)
		}
		routes, err := queue.ListRoutes(ctx, pool)
		if err != nil {
			return err
		}
		return printRoutes(os.Stdout, routes, *asJSON)
	case "set":
		if len(pos) != 2 {
			return errors.New(routesUsage)
		}
		jobType, executor := pos[0], pos[1]
		if executor == queue.ExecutorGo && !queue.HasExecutor(jobType) && !*force {
			fmt.Fprintf(os.Stderr, `refusing to route %s to go: this binary has no Go executor for it, so nothing
would claim its jobs (Python stops claiming a type the moment it is routed away).

Job types this binary can execute: %s.
Use --force only for a type whose executor is provided by a different build of
opengtm that is running as a worker.
`, jobType, strings.Join(queue.Executors(), ", "))
			return errors.New("route unchanged")
		}
		route, err := queue.SetRoute(ctx, pool, jobType, executor, *force)
		var inflight *queue.InFlightError
		if errors.As(err, &inflight) {
			fmt.Fprintf(os.Stderr, `refusing to route %s to %s: %d job(s) are processing under the current executor.

Drain first so two executors never work the same type:
  1. stop the current executor from claiming new %s jobs (scale it down or
     restart it without that handler);
  2. wait until "opengtm routes list" shows 0 processing for %s (stuck
     attempts are recovered by the heartbeat reaper within ~5 minutes);
  3. run this command again.
Use --force only if those attempts are known to be dead.
`, jobType, executor, inflight.Processing, jobType, jobType)
			return errors.New("route unchanged")
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s -> %s (pending %d, processing %d)\n", route.JobType, route.Executor, route.Pending, route.Processing)
		if executor == queue.ExecutorGo {
			fmt.Println("note: Python workers older than migration 7c1e5a9d3b20 ignore routes; upgrade them first.")
		}
		return nil
	default:
		return errors.New(routesUsage)
	}
}

// parseInterleaved lets flags follow positional arguments
// (`routes set plugin_run go --force`), which flag.Parse alone stops at.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w\n%s", err, routesUsage)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func printRoutes(w io.Writer, routes []queue.Route, asJSON bool) error {
	if asJSON {
		if routes == nil {
			routes = []queue.Route{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(routes)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB TYPE\tEXECUTOR\tPENDING\tPROCESSING\tUPDATED")
	for _, r := range routes {
		executor, updated := r.Executor, "-"
		if r.Default {
			executor += " (default, switchable)"
		}
		if r.Executor == queue.ExecutorGo && !r.GoExecutor {
			executor += " (NO EXECUTOR IN THIS BINARY: jobs are not claimed)"
		}
		if !r.UpdatedAt.IsZero() {
			updated = r.UpdatedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", r.JobType, executor, r.Pending, r.Processing, updated)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "\nJob types not listed are claimed by Python workers.")
	return err
}
