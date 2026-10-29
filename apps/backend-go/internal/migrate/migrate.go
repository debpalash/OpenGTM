// Package migrate owns the PostgreSQL schema: a goose (v3) runner over
// migrations embedded in the binary. Version 1 is the baseline (the schema at
// Alembic revision e7b2c4d6f8a0, the last legacy/python head); later versions
// are SQL files under migrations/.
//
// An empty database gets the baseline. A database Alembic left at exactly
// e7b2c4d6f8a0 is adopted (version 1 recorded, nothing re-run). Anything else
// is refused. Runs serialize on a session advisory lock, so concurrent
// `opengtm-migrate` processes are safe.
//
// alembic_version stays stamped at e7b2c4d6f8a0 so the Python API (readiness
// head check, go_executor) keeps working against a Go-migrated database during
// the canary; Go binaries check goose_db_version instead (CheckSchema).
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
)

const (
	// BaselineVersion is the goose version of the baseline migration.
	BaselineVersion int64 = 1
	// BaselineAlembicRevision is the Alembic revision the baseline reproduces.
	// The runner stamps it on fresh databases and adopts databases at it.
	BaselineAlembicRevision = "e7b2c4d6f8a0"
	// VersionTable is goose's bookkeeping table (public schema).
	VersionTable = "goose_db_version"
	// LockKey is the pg_advisory_lock key held for a whole run ("OGTMMIGR").
	LockKey int64 = 0x4f47544d4d494752
	// DefaultAppRole is the NOLOGIN group role the runtime login belongs to
	// (YUPCHA_APP_DB_ROLE, same default as the Alembic migrations).
	DefaultAppRole = "yupcha_app"
)

//go:embed baseline/e7b2c4d6f8a0.sql
var baselineSQL string

//go:embed migrations
var migrationsFS embed.FS

// Options configures a run.
type Options struct {
	// AppRole receives the runtime grants (created NOLOGIN if missing). Empty = DefaultAppRole.
	AppRole string
	// RuntimeLogin, when set, creates or updates that login role and makes it a
	// member of AppRole (Python's YUPCHA_PROVISION_RUNTIME_ROLE).
	RuntimeLogin *RuntimeLogin
	Logger       *slog.Logger
}

// RuntimeLogin is the concrete least-privilege login the services connect as.
type RuntimeLogin struct {
	User     string
	Password string
}

// Result describes what a run did.
type Result struct {
	From, To int64   // goose version before and after (0 = none)
	Adopted  bool    // an Alembic-managed database was adopted at the baseline
	Applied  []int64 // versions applied (the baseline counts even when adopted)
}

var roleRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validRole(r string) error {
	if !roleRe.MatchString(r) {
		return fmt.Errorf("migrate: %q is not a safe PostgreSQL role identifier", r)
	}
	return nil
}

// Run applies every pending migration under the advisory lock, then
// provisions the runtime login when requested.
func Run(ctx context.Context, db *sql.DB, o Options) (Result, error) {
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	role := o.AppRole
	if role == "" {
		role = DefaultAppRole
	}
	if err := validRole(role); err != nil {
		return Result{}, fmt.Errorf("%w (YUPCHA_APP_DB_ROLE)", err)
	}
	if rl := o.RuntimeLogin; rl != nil {
		if err := validRole(rl.User); err != nil {
			return Result{}, fmt.Errorf("%w (YUPCHA_RUNTIME_DB_USER)", err)
		}
		if rl.Password == "" {
			return Result{}, errors.New("migrate: YUPCHA_RUNTIME_DB_PASSWORD must be set when provisioning the runtime login")
		}
	}

	lock, err := db.Conn(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("migrate: connect: %w", err)
	}
	defer lock.Close()
	log.Info("migrate: waiting for advisory lock", "key", LockKey)
	if _, err := lock.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, LockKey); err != nil {
		return Result{}, fmt.Errorf("migrate: advisory lock: %w", err)
	}
	defer func() { _, _ = lock.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, LockKey) }()

	st, err := inspect(ctx, db)
	if err != nil {
		return Result{}, err
	}
	p, err := decide(st)
	if err != nil {
		return Result{}, err
	}
	res := Result{From: max(st.gooseVersion, 0), Adopted: p == planAdopt}
	switch p {
	case planFresh:
		log.Info("migrate: empty database, applying baseline", "alembic_revision", BaselineAlembicRevision)
	case planAdopt:
		log.Info("migrate: adopting Alembic-managed database", "alembic_revision", BaselineAlembicRevision)
	}

	prov, err := newProvider(db, role)
	if err != nil {
		return res, err
	}
	results, err := prov.Up(ctx)
	if err != nil {
		return res, fmt.Errorf("migrate: up: %w", err)
	}
	for _, r := range results {
		res.Applied = append(res.Applied, r.Source.Version)
		name := r.Source.Path
		if name == "" {
			name = "baseline"
		}
		log.Info("migrate: applied", "version", r.Source.Version, "name", name, "duration", r.Duration)
	}
	if res.To, err = prov.GetDBVersion(ctx); err != nil {
		return res, fmt.Errorf("migrate: read version: %w", err)
	}
	if o.RuntimeLogin != nil {
		if err := provisionRuntimeLogin(ctx, db, role, *o.RuntimeLogin); err != nil {
			return res, err
		}
		log.Info("migrate: runtime login provisioned", "user", o.RuntimeLogin.User, "group", role)
	}
	return res, nil
}

func newProvider(db *sql.DB, role string) (*goose.Provider, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub,
		goose.WithDisableGlobalRegistry(true),
		goose.WithGoMigrations(goose.NewGoMigration(BaselineVersion, &goose.GoFunc{RunTx: baselineUp(role)}, nil)),
	)
	if err != nil {
		return nil, fmt.Errorf("migrate: provider: %w", err)
	}
	return p, nil
}

// LatestVersion is the highest version embedded in this binary.
func LatestVersion() (int64, error) {
	latest := BaselineVersion
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		v, err := goose.NumericComponent(e.Name())
		if err != nil {
			return 0, fmt.Errorf("migrate: %s: %w", e.Name(), err)
		}
		latest = max(latest, v)
	}
	return latest, nil
}

// VersionStatus is one embedded migration and whether the database has it.
type VersionStatus struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt time.Time
}

// Status lists the embedded migrations with their applied state. Read only:
// unlike goose's own Status it never creates the version table.
func Status(ctx context.Context, db *sql.DB) ([]VersionStatus, error) {
	out := []VersionStatus{{Version: BaselineVersion, Name: "baseline (Alembic " + BaselineAlembicRevision + ")"}}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		v, err := goose.NumericComponent(e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrate: %s: %w", e.Name(), err)
		}
		out = append(out, VersionStatus{Version: v, Name: e.Name()})
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.`+VersionTable+`') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("migrate: status: %w", err)
	}
	if !exists {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT version_id, max(tstamp) FROM public.`+VersionTable+` WHERE version_id > 0 GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("migrate: status: %w", err)
	}
	defer rows.Close()
	applied := map[int64]time.Time{}
	for rows.Next() {
		var v int64
		var ts time.Time
		if err := rows.Scan(&v, &ts); err != nil {
			return nil, err
		}
		applied[v] = ts
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].AppliedAt, out[i].Applied = applied[out[i].Version]
	}
	return out, nil
}

type plan int

const (
	planGoose plan = iota + 1 // already Go-managed: apply whatever is pending
	planFresh                 // empty database: baseline runs
	planAdopt                 // Alembic at the baseline revision: record version 1 only
)

func (p plan) String() string {
	return [...]string{"?", "goose", "fresh", "adopt"}[p]
}

type dbState struct {
	gooseVersion int64 // -1: no goose table
	alembicTable bool
	alembic      []string
	tables       int // public tables/views/sequences other than the two version tables
}

func inspect(ctx context.Context, db *sql.DB) (dbState, error) {
	st := dbState{gooseVersion: -1}
	var hasGoose bool
	err := db.QueryRowContext(ctx, `SELECT to_regclass('public.`+VersionTable+`') IS NOT NULL, to_regclass('public.alembic_version') IS NOT NULL`).
		Scan(&hasGoose, &st.alembicTable)
	if err != nil {
		return st, fmt.Errorf("migrate: inspect: %w", err)
	}
	if hasGoose {
		if err := db.QueryRowContext(ctx, `SELECT coalesce(max(version_id), 0) FROM public.`+VersionTable).Scan(&st.gooseVersion); err != nil {
			return st, fmt.Errorf("migrate: read %s: %w", VersionTable, err)
		}
	}
	if st.alembicTable {
		rows, err := db.QueryContext(ctx, `SELECT version_num FROM public.alembic_version ORDER BY 1`)
		if err != nil {
			return st, fmt.Errorf("migrate: read alembic_version: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return st, err
			}
			st.alembic = append(st.alembic, v)
		}
		if err := rows.Err(); err != nil {
			return st, err
		}
	}
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p','v','m','f')
		  AND c.relname NOT IN ('`+VersionTable+`', 'alembic_version')`).Scan(&st.tables)
	if err != nil {
		return st, fmt.Errorf("migrate: inspect tables: %w", err)
	}
	return st, nil
}

func decide(st dbState) (plan, error) {
	if st.gooseVersion >= BaselineVersion {
		return planGoose, nil
	}
	if st.alembicTable {
		switch {
		case len(st.alembic) == 1 && st.alembic[0] == BaselineAlembicRevision:
			return planAdopt, nil
		case len(st.alembic) == 0:
			return 0, errors.New("migrate: refusing: alembic_version is empty, so the schema state is unknown; " +
				"restore the stamp or upgrade with the legacy tool (legacy/python: alembic upgrade " + BaselineAlembicRevision + ") first")
		default:
			return 0, fmt.Errorf("migrate: refusing: database is at Alembic revision %q, but the Go runner can only adopt %q. "+
				"Upgrade it with the legacy tool first (legacy/python at commit 86f41c6 or later: `alembic upgrade %s`), then run opengtm-migrate again",
				strings.Join(st.alembic, ","), BaselineAlembicRevision, BaselineAlembicRevision)
		}
	}
	if st.tables > 0 {
		return 0, fmt.Errorf("migrate: refusing: the public schema has %d tables but is not managed by Alembic or goose "+
			"(no alembic_version / %s); point DATABASE_URL at an empty database or an Alembic database at %s",
			st.tables, VersionTable, BaselineAlembicRevision)
	}
	return planFresh, nil
}
