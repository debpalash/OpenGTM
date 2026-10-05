package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/migrate"
)

func init() {
	register("migrate", "apply database migrations as the single schema owner (Alembic engine)", runMigrate)
}

func runMigrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dir := installFlags(fs)
	dbURL := fs.String("database-url", "", "owner database URL (default: the install's postgres service, else $OPENGTM_ADMIN_DATABASE_URL / $DATABASE_URL)")
	executor := fs.String("executor", "auto", "where Alembic runs: auto, compose (the install's migrate service) or python (a local checkout or image)")
	python := fs.String("python", "python", "interpreter command for --executor python, e.g. \"uv run --frozen python\"")
	root := fs.String("root", "", "repository root with alembic.ini for --executor python (default: current directory, $OPENGTM_ROOT, /app)")
	to := fs.String("to", "head", "target revision (default head); a partial target skips runtime-role provisioning")
	plan := fs.Bool("plan", false, "show what would change and exit")
	lockWait := fs.Duration("lock-timeout", 5*time.Minute, "how long to wait for another migration owner")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, inst, release, err := connFor(*dir, *dbURL, os.Stderr)
	if err != nil {
		return err
	}
	defer release()

	mode := *executor
	if mode == "auto" {
		mode = "python"
		if inst != nil {
			mode = "compose"
		}
	}

	var engine migrate.Engine
	switch mode {
	case "compose":
		if inst == nil {
			return fmt.Errorf("--executor compose needs an install directory; %s is not one (or --database-url was given)", *dir)
		}
		p := projectOf(inst, os.Stderr)
		if err := p.Up(ctx, 3*time.Minute, "postgres"); err != nil {
			return fmt.Errorf("start PostgreSQL: %w", err)
		}
		engine = migrate.ComposeEngine(p)
	case "python":
		if db.IsExec() {
			return fmt.Errorf("--executor python needs a database URL the host can reach; pass --database-url")
		}
		r, ok := migrate.FindRoot(*root, os.Getenv("OPENGTM_ROOT"), ".", "/app")
		if !ok {
			return fmt.Errorf("--executor python: no alembic.ini and apps/api/scripts/migrate.py found; run from the repository (or the Python image's /app) or pass --root")
		}
		engine = migrate.LocalEngine(r, strings.Fields(*python), db.URL, execx.OS{})
	default:
		return fmt.Errorf("unknown --executor %q (auto, compose, python)", *executor)
	}

	// One owner at a time: an advisory lock in the database when we can reach
	// it directly (covers several hosts), plus the install lock taken above.
	if !db.IsExec() && !*plan {
		unlock, err := migrate.Lock(ctx, db.URL, *lockWait)
		if err != nil {
			return err
		}
		defer unlock()
	}
	_, err = migrate.Apply(ctx, migrate.Options{DB: db, Engine: engine, Target: *to, Plan: *plan, Out: os.Stdout})
	return err
}
