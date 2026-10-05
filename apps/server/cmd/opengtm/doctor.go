package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/process"
	"github.com/debpalash/OpenGTM/apps/server/internal/pluginrun"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

func init() {
	register("doctor", "check configuration, database role, schema and legacy API", runDoctor)
}

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func runDoctor(ctx context.Context, args []string) error {
	fs, cfgPath := roleFlags("doctor")
	asJSON := fs.Bool("json", false, "print results as JSON")
	installDir := fs.String("dir", "", "check the Compose install in this directory from the host (Docker, files, containers, database)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var results []checkResult
	if *installDir != "" {
		results = installChecks(ctx, *installDir)
	} else {
		results = doctorChecks(ctx, *cfgPath)
	}
	if err := printChecks(os.Stdout, results, *asJSON); err != nil {
		return err
	}
	for _, r := range results {
		if !r.OK {
			return errors.New("doctor found problems")
		}
	}
	return nil
}

func doctorChecks(ctx context.Context, cfgPath string) []checkResult {
	var out []checkResult
	add := func(name string, ok bool, format string, a ...any) {
		out = append(out, checkResult{Name: name, OK: ok, Detail: fmt.Sprintf(format, a...)})
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		add("config", false, "%v", err)
		return out
	}
	add("config", true, "valid")
	out = append(out, processChecks(ctx, cfg)...)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL, db.Options{AppName: "opengtm-doctor", MaxConns: 2})
	if err != nil {
		add("database", false, "%v", err)
	} else {
		defer pool.Close()
		out = append(out, databaseChecks(ctx, pool)...)
	}
	out = append(out, legacyCheck(ctx, cfg.LegacyAPIURL))
	return out
}

// processChecks verifies the host can run the process (Python) plugins in the
// catalog: interpreter, SDK, state directory and sandbox. It adds nothing when
// no process plugin is installed.
func processChecks(ctx context.Context, cfg config.Config) []checkResult {
	if !pluginrun.LoadCatalog(cfg.Plugins).HasRuntime("process") {
		return nil
	}
	opts, err := process.OptionsFromEnv(process.Options{}, nil)
	if err != nil {
		return []checkResult{{"process plugins", false, err.Error()}}
	}
	var out []checkResult
	for _, c := range process.Preflight(ctx, opts) {
		out = append(out, checkResult{c.Name, c.OK, c.Detail})
	}
	return out
}

func databaseChecks(ctx context.Context, pool *pgxpool.Pool) []checkResult {
	var out []checkResult
	var user, dbName, server string
	var super, bypass bool
	err := pool.QueryRow(ctx, `
SELECT current_user, current_database(), current_setting('server_version'), r.rolsuper, r.rolbypassrls
FROM pg_roles r WHERE r.rolname = current_user`).Scan(&user, &dbName, &server, &super, &bypass)
	if err != nil {
		return append(out, checkResult{"database", false, err.Error()})
	}
	out = append(out, checkResult{"database", true,
		fmt.Sprintf("connected to %s as %s (PostgreSQL %s)", dbName, user, server)})

	switch {
	case super || bypass:
		attr := "SUPERUSER"
		if !super {
			attr = "BYPASSRLS"
		}
		out = append(out, checkResult{"runtime role", false, fmt.Sprintf(
			"%s has %s, so PostgreSQL skips row-level security and tenant isolation would rest on "+
				"application code alone. Run opengtm as a NOSUPERUSER NOBYPASSRLS login role that is a "+
				"member of the runtime group (APP_DB_ROLE, default yupcha_app); keep the owner role for migrations.",
			user, attr)})
	default:
		out = append(out, checkResult{"runtime role", true, user + " is NOSUPERUSER NOBYPASSRLS"})
	}

	var revision *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.alembic_version')::text`).Scan(&revision); err != nil || revision == nil {
		out = append(out, checkResult{"migrations", false,
			"alembic_version table not found; run `alembic upgrade head` with the owner role"})
	} else {
		var versions []string
		rows, err := pool.Query(ctx, `SELECT version_num FROM alembic_version`)
		if err == nil {
			for rows.Next() {
				var v string
				if rows.Scan(&v) == nil {
					versions = append(versions, v)
				}
			}
			err = rows.Err()
		}
		switch {
		case err != nil:
			out = append(out, checkResult{"migrations", false, err.Error()})
		case len(versions) == 0:
			out = append(out, checkResult{"migrations", false, "alembic_version is empty; the schema was never migrated"})
		default:
			out = append(out, checkResult{"migrations", true, fmt.Sprintf("alembic revision %v", versions)})
		}
	}

	var routes int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM job_executor_routes`).Scan(&routes)
	if err != nil {
		out = append(out, checkResult{"executor routes", false,
			"job_executor_routes is missing or unreadable (" + err.Error() + "); migrate to revision 7c1e5a9d3b20 or later"})
	} else {
		out = append(out, checkResult{"executor routes", true, fmt.Sprintf("%d explicit route(s)", routes)})
		out = append(out, executorCheck(ctx, pool))
	}
	return out
}

// executorCheck flags a type routed to go that this binary cannot execute:
// Python no longer claims it and no Go worker built from this binary will, so
// its jobs wait forever. It also lists the types available to switch.
func executorCheck(ctx context.Context, pool *pgxpool.Pool) checkResult {
	routes, err := queue.ListRoutes(ctx, pool)
	if err != nil {
		return checkResult{"go executors", false, err.Error()}
	}
	var stranded, inGo, available []string
	for _, r := range routes {
		switch {
		case r.Executor == queue.ExecutorGo && !r.GoExecutor:
			stranded = append(stranded, fmt.Sprintf("%s (%d pending, %d processing)", r.JobType, r.Pending, r.Processing))
		case r.Executor == queue.ExecutorGo:
			inGo = append(inGo, r.JobType)
		case r.Default:
			available = append(available, r.JobType)
		}
	}
	if len(stranded) > 0 {
		return checkResult{"go executors", false, "routed to go but not executable by this binary, so nothing claims them: " +
			strings.Join(stranded, ", ") + ". Run a worker built with that executor, or `opengtm routes set <type> python`."}
	}
	detail := fmt.Sprintf("%d type(s) running in go", len(inGo))
	if len(inGo) > 0 {
		detail += " (" + strings.Join(inGo, ", ") + ")"
	}
	if len(available) > 0 {
		detail += "; python-owned and switchable: " + strings.Join(available, ", ")
	}
	return checkResult{"go executors", true, detail}
}

func legacyCheck(ctx context.Context, base string) checkResult {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return checkResult{"legacy api", false, err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checkResult{"legacy api", false, fmt.Sprintf("%s unreachable: %v", base, err)}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checkResult{"legacy api", false, fmt.Sprintf("%s/health returned %s", base, resp.Status)}
	}
	return checkResult{"legacy api", true, base + " healthy"}
}

func printChecks(w io.Writer, results []checkResult, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tSTATUS\tDETAIL")
	for _, r := range results {
		status := "ok"
		if !r.OK {
			status = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, status, r.Detail)
	}
	return tw.Flush()
}
