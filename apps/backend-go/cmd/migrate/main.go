// Command opengtm-migrate owns the PostgreSQL schema (internal/migrate):
//
//	DATABASE_URL=... opengtm-migrate [up]   apply pending migrations (default)
//	DATABASE_URL=... opengtm-migrate status list embedded versions and whether each is applied
//	opengtm-migrate version                 print the embedded versions (no database)
//
// DATABASE_URL must carry owner credentials (DDL, CREATE ROLE when the app role is
// missing); a SQLAlchemy "postgresql+psycopg://" URL is accepted. Environment, as
// for the legacy Python migrate job: YUPCHA_APP_DB_ROLE (default yupcha_app),
// YUPCHA_PROVISION_RUNTIME_ROLE=1 with YUPCHA_RUNTIME_DB_USER (default
// yupcha_runtime) and YUPCHA_RUNTIME_DB_PASSWORD. Safe to run concurrently.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/migrate"
)

const usage = "usage: opengtm-migrate [up|status|version]"

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "opengtm-migrate:", err)
		os.Exit(1)
	}
}

var driverSuffix = regexp.MustCompile(`^(postgres(?:ql)?)\+[A-Za-z0-9_]+://`)

// normalizeURL turns a SQLAlchemy URL (postgresql+psycopg://) into a libpq one.
func normalizeURL(u string) string { return driverSuffix.ReplaceAllString(u, "$1://") }

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func optionsFromEnv(getenv func(string) string) (migrate.Options, error) {
	o := migrate.Options{AppRole: getenv("YUPCHA_APP_DB_ROLE")}
	if o.AppRole == "" {
		o.AppRole = migrate.DefaultAppRole
	}
	if truthy(getenv("YUPCHA_PROVISION_RUNTIME_ROLE")) {
		rl := &migrate.RuntimeLogin{User: getenv("YUPCHA_RUNTIME_DB_USER"), Password: getenv("YUPCHA_RUNTIME_DB_PASSWORD")}
		if rl.User == "" {
			rl.User = "yupcha_runtime"
		}
		if rl.Password == "" {
			return o, errors.New("YUPCHA_RUNTIME_DB_PASSWORD must be set when YUPCHA_PROVISION_RUNTIME_ROLE is enabled")
		}
		o.RuntimeLogin = rl
	}
	return o, nil
}

func run(args []string, getenv func(string) string, out io.Writer) error {
	cmd := "up"
	if len(args) > 0 {
		cmd = args[0]
	}
	if len(args) > 1 {
		return errors.New(usage)
	}
	switch cmd {
	case "up", "status":
	case "version":
		latest, err := migrate.LatestVersion()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "latest=%d baseline=%d alembic_baseline=%s\n", latest, migrate.BaselineVersion, migrate.BaselineAlembicRevision)
		return nil
	default:
		return errors.New(usage)
	}
	dsn := getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required (owner credentials)")
	}
	opts, err := optionsFromEnv(getenv)
	if err != nil {
		return err
	}
	opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := sql.Open("pgx", normalizeURL(dsn))
	if err != nil {
		return errors.New("cannot parse DATABASE_URL") // never echo the URL: it carries the password
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("cannot connect to the database: %w", err)
	}

	if cmd == "status" {
		st, err := migrate.Status(ctx, db)
		if err != nil {
			return err
		}
		for _, s := range st {
			applied := "pending"
			if s.Applied {
				applied = "applied " + s.AppliedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			fmt.Fprintf(out, "%d\t%s\t%s\n", s.Version, s.Name, applied)
		}
		return nil
	}
	res, err := migrate.Run(ctx, db, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "from=%d to=%d applied=%v adopted=%t\n", res.From, res.To, res.Applied, res.Adopted)
	return nil
}
