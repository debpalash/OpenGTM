package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/importv3"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/migrate"
)

const importUsage = "usage: opengtm-migrate import-v3 --data-dir DIR [--dry-run] [--allow-missing-workspaces]"

// runImportV3 copies a v3.0.0 installation's SQLite stores into PostgreSQL (internal/importv3). Run it after
// `opengtm-migrate up` has bridged the schema and before the Go services start.
func runImportV3(args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("import-v3", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("data-dir", getenv("V3_DATA_DIR"), "the v3.0.0 data/ directory (or V3_DATA_DIR)")
	dry := fs.Bool("dry-run", false, "read, import and verify inside a transaction, print counts, roll back")
	allow := fs.Bool("allow-missing-workspaces", false, "accept a data dir without workspaces.db")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errors.New(importUsage)
	}
	if *dir == "" {
		return errors.New("import-v3: --data-dir is required; " + importUsage)
	}
	dsn := getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required (owner credentials)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := sql.Open("pgx", normalizeURL(dsn))
	if err != nil {
		return errors.New("cannot parse DATABASE_URL")
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("cannot connect to the database: %w", err)
	}
	// The import presumes the schema is at the Go baseline or later (the bridge has run).
	var v int64
	if err := db.QueryRowContext(ctx, `SELECT coalesce(max(version_id), 0) FROM public.`+migrate.VersionTable).Scan(&v); err != nil || v < migrate.BaselineVersion {
		return errors.New("import-v3: the schema is not migrated yet: run `opengtm-migrate up` first")
	}
	rep, err := importv3.Run(ctx, db, importv3.Options{DataDir: *dir, DryRun: *dry, AllowMissingWorkspaces: *allow, Out: out})
	rep.Print(out)
	return err
}
