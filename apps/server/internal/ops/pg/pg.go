// Package pg runs the PostgreSQL client tools (psql, pg_dump, pg_restore)
// against either a database URL or the postgres service of a Compose install.
//
// Backups deliberately go through the official client tools rather than a
// reimplementation: pg_dump's custom format is the one every DBA can inspect
// and restore with stock tooling, so a backup stays usable without opengtm.
//
// Two connection modes share one API:
//
//   - direct: tools run on this host and connect with a postgresql:// URL.
//     The password travels in PGPASSWORD, never on the command line.
//   - exec: tools run inside the Compose `postgres` container (`docker compose
//     exec`), which always matches the server's major version and needs no
//     published port.
package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/compose"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
)

const sep = "\x1f"

// Conn identifies a database and how to reach it.
type Conn struct {
	// URL is a libpq URL (direct mode). SQLAlchemy driver suffixes are accepted.
	URL string

	// Exec mode: run tools inside the Compose service.
	Compose  *compose.Project
	Service  string // default "postgres"
	User     string
	Database string

	Run execx.Runner
}

// Direct builds a direct-mode Conn from any URL the Python or Go stacks accept.
func Direct(rawURL string, run execx.Runner) (Conn, error) {
	u, err := config.NormalizeDatabaseURL(rawURL)
	if err != nil {
		return Conn{}, err
	}
	return Conn{URL: u, Run: run}, nil
}

func (c Conn) runner() execx.Runner {
	if c.Run == nil {
		return execx.OS{}
	}
	return c.Run
}

// IsExec reports whether tools run inside a container.
func (c Conn) IsExec() bool { return c.Compose != nil }

// DatabaseName is the database this connection targets.
func (c Conn) DatabaseName() string {
	if c.IsExec() {
		return c.Database
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// Describe renders the target for logs without credentials.
func (c Conn) Describe() string {
	if c.IsExec() {
		return fmt.Sprintf("compose service %s, database %s", c.service(), c.Database)
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return "database"
	}
	return fmt.Sprintf("%s%s", u.Host, u.Path)
}

// WithDatabase returns the same server with another database selected.
func (c Conn) WithDatabase(name string) Conn {
	out := c
	if c.IsExec() {
		out.Database = name
		return out
	}
	if u, err := url.Parse(c.URL); err == nil {
		u.Path = "/" + name
		out.URL = u.String()
	}
	return out
}

func (c Conn) service() string {
	if c.Service != "" {
		return c.Service
	}
	return "postgres"
}

// splitPassword removes the password from a URL so it can go in PGPASSWORD.
func splitPassword(raw string) (clean, password string) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw, ""
	}
	pw, _ := u.User.Password()
	u.User = url.User(u.User.Username())
	return u.String(), pw
}

// tool builds the command for one client tool. extra holds tool arguments;
// the connection arguments are added in the right form for the mode.
func (c Conn) tool(name string, stdin io.Reader, extra ...string) execx.Cmd {
	if c.IsExec() {
		args := []string{"exec", "-T", c.service(), name, "--username=" + c.User, "--dbname=" + c.Database}
		cmd := c.Compose.Cmd(append(args, extra...)...)
		cmd.Stdin = stdin
		return cmd
	}
	clean, pw := splitPassword(c.URL)
	cmd := execx.Cmd{Name: name, Args: append([]string{"--dbname=" + clean}, extra...), Stdin: stdin}
	if pw != "" {
		cmd.Env = []string{"PGPASSWORD=" + pw}
	}
	return cmd
}

// ErrToolMissing wraps a missing client tool with an install hint.
var ErrToolMissing = errors.New("PostgreSQL client tool not found on PATH")

func (c Conn) wrap(err error, tool string) error {
	if err != nil && execx.Missing(err) {
		return fmt.Errorf("%w: %s (install postgresql-client, or run against a Compose install so the tool runs in the container): %v",
			ErrToolMissing, tool, err)
	}
	return err
}

// Query runs sql through psql and returns rows of unit-separated columns.
func (c Conn) Query(ctx context.Context, sql string) ([][]string, error) {
	cmd := c.tool("psql", nil, "--no-psqlrc", "--quiet", "--tuples-only", "--no-align",
		"--field-separator="+sep, "--set=ON_ERROR_STOP=1", "--command="+sql)
	out, err := execx.Output(ctx, c.runner(), cmd)
	if err != nil {
		return nil, c.wrap(err, "psql")
	}
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rows = append(rows, strings.Split(line, sep))
	}
	return rows, nil
}

// Exec runs one or more statements, failing on the first error.
func (c Conn) Exec(ctx context.Context, sql string) error {
	cmd := c.tool("psql", nil, "--no-psqlrc", "--quiet", "--set=ON_ERROR_STOP=1", "--command="+sql)
	return c.wrap(execx.Do(ctx, c.runner(), cmd), "psql")
}

// ServerVersion returns server_version, e.g. "18.6 (Debian ...)".
func (c Conn) ServerVersion(ctx context.Context) (string, error) {
	rows, err := c.Query(ctx, "SELECT current_setting('server_version')")
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0][0], nil
}

// Revisions returns the Alembic revision(s) stored in the database, or nil
// when the schema was never migrated.
func (c Conn) Revisions(ctx context.Context) ([]string, error) {
	rows, err := c.Query(ctx, "SELECT to_regclass('public.alembic_version') IS NOT NULL")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0][0] != "t" {
		return nil, nil
	}
	rows, err = c.Query(ctx, "SELECT version_num FROM public.alembic_version ORDER BY 1")
	if err != nil {
		return nil, err
	}
	var revs []string
	for _, r := range rows {
		revs = append(revs, r[0])
	}
	return revs, nil
}

// TableCount counts user tables (ordinary and partitioned) outside system schemas.
func (c Conn) TableCount(ctx context.Context) (int, error) {
	rows, err := c.Query(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p') AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'`)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return strconv.Atoi(rows[0][0])
}

// IsEmpty reports whether the database holds no relations (tables, views,
// materialized views or sequences) outside system schemas.
func (c Conn) IsEmpty(ctx context.Context) (bool, error) {
	rows, err := c.Query(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','v','m','S','f') AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'`)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	return rows[0][0] == "0", nil
}

// Dump streams a custom-format pg_dump to w.
func (c Conn) Dump(ctx context.Context, w io.Writer) error {
	cmd := c.tool("pg_dump", nil, "--format=custom", "--no-password")
	cmd.Stdout = w
	return c.wrap(execx.Do(ctx, c.runner(), cmd), "pg_dump")
}

// Restore loads a custom-format dump from r in one transaction, so a failed
// restore leaves the database exactly as it was before the attempt.
func (c Conn) Restore(ctx context.Context, r io.Reader) error {
	cmd := c.tool("pg_restore", r, "--no-owner", "--single-transaction", "--exit-on-error", "--no-password")
	return c.wrap(execx.Do(ctx, c.runner(), cmd), "pg_restore")
}

// Wipe drops every user schema and recreates an empty public schema with the
// stock ownership and grants, so a following Restore reproduces exactly the
// dumped state. Objects created after the backup (new tables from a later
// migration) disappear too; `pg_restore --clean` alone would leave them.
func (c Conn) Wipe(ctx context.Context) error {
	return c.Exec(ctx, `
DO $$
DECLARE s text;
BEGIN
  FOR s IN SELECT nspname FROM pg_namespace
           WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema' LOOP
    EXECUTE format('DROP SCHEMA %I CASCADE', s);
  END LOOP;
  IF current_setting('server_version_num')::int >= 150000 THEN
    EXECUTE 'CREATE SCHEMA public AUTHORIZATION pg_database_owner';
    EXECUTE 'GRANT USAGE ON SCHEMA public TO PUBLIC';
  ELSE
    EXECUTE 'CREATE SCHEMA public';
    EXECUTE 'GRANT ALL ON SCHEMA public TO PUBLIC';
  END IF;
  EXECUTE 'COMMENT ON SCHEMA public IS ''standard public schema''';
END $$;`)
}

// RolesSQL renders an idempotent script that recreates the cluster's
// non-superuser roles and their memberships, without passwords. pg_dump does
// not carry roles, yet policies and grants in the dump reference them, so a
// restore into a fresh cluster fails without this. Passwords are re-applied by
// the migration step, which re-provisions the runtime login from .env.
func (c Conn) RolesSQL(ctx context.Context) (string, error) {
	roles, err := c.Query(ctx, `SELECT rolname, rolcanlogin::int, rolbypassrls::int, rolcreatedb::int, rolcreaterole::int, rolinherit::int
FROM pg_roles WHERE rolname !~ '^pg_' AND NOT rolsuper ORDER BY 1`)
	if err != nil {
		return "", err
	}
	members, err := c.Query(ctx, `SELECT g.rolname, m.rolname FROM pg_auth_members am
JOIN pg_roles g ON g.oid = am.roleid JOIN pg_roles m ON m.oid = am.member
WHERE g.rolname !~ '^pg_' AND m.rolname !~ '^pg_' AND NOT g.rolsuper AND NOT m.rolsuper ORDER BY 1, 2`)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	b.WriteString("-- Roles recreated by `opengtm restore`; no passwords are stored in backups.\n")
	flag := func(v, on, off string) string {
		if v == "1" {
			return on
		}
		return off
	}
	for _, r := range roles {
		if len(r) < 6 {
			continue
		}
		attrs := strings.Join([]string{
			flag(r[1], "LOGIN", "NOLOGIN"), "NOSUPERUSER",
			flag(r[2], "BYPASSRLS", "NOBYPASSRLS"),
			flag(r[3], "CREATEDB", "NOCREATEDB"),
			flag(r[4], "CREATEROLE", "NOCREATEROLE"),
			flag(r[5], "INHERIT", "NOINHERIT"),
		}, " ")
		fmt.Fprintf(&b, "DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN CREATE ROLE %s %s; END IF; END $$;\n",
			literal(r[0]), pgx.Identifier{r[0]}.Sanitize(), attrs)
	}
	for _, m := range members {
		if len(m) < 2 {
			continue
		}
		fmt.Fprintf(&b, "GRANT %s TO %s;\n", pgx.Identifier{m[0]}.Sanitize(), pgx.Identifier{m[1]}.Sanitize())
	}
	return b.String(), nil
}

// ApplyRoles runs a RolesSQL script. Individual failures (for example a
// managed service that forbids CREATE ROLE) are returned as warnings, not
// errors: the restore itself reports precisely what it cannot resolve.
func (c Conn) ApplyRoles(ctx context.Context, script string) (warnings []string, err error) {
	cmd := c.tool("psql", strings.NewReader(script), "--no-psqlrc", "--quiet", "--file=-")
	var out bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &out
	if runErr := c.runner().Run(ctx, cmd); runErr != nil {
		return nil, c.wrap(&execx.Error{Cmd: cmd.String(), Err: runErr, Stderr: out.String()}, "psql")
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "ERROR") {
			warnings = append(warnings, strings.TrimSpace(line))
		}
	}
	return warnings, nil
}

// CreateDatabase creates name on the same server (via the postgres database).
func (c Conn) CreateDatabase(ctx context.Context, name string) (Conn, error) {
	if err := c.WithDatabase("postgres").Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return Conn{}, err
	}
	return c.WithDatabase(name), nil
}

// DropDatabase drops name on the same server, disconnecting other sessions.
func (c Conn) DropDatabase(ctx context.Context, name string) error {
	return c.WithDatabase("postgres").Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
