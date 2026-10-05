package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/backup"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/install"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/upgrade"
)

// installChecks is `opengtm doctor --dir`: it inspects a Compose install from
// the host (Docker, files, containers, the database through the postgres
// container) instead of from inside the stack.
func installChecks(ctx context.Context, dir string) []checkResult {
	var out []checkResult
	add := func(name string, ok bool, format string, a ...any) {
		out = append(out, checkResult{Name: name, OK: ok, Detail: fmt.Sprintf(format, a...)})
	}

	if v, err := execx.Output(ctx, execx.OS{}, execx.Cmd{Name: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"}}); err != nil {
		add("docker", false, "docker is not reachable: %v", err)
		return out
	} else {
		add("docker", true, "engine %s", v)
	}
	if v, err := execx.Output(ctx, execx.OS{}, execx.Cmd{Name: "docker", Args: []string{"compose", "version", "--short"}}); err != nil {
		add("compose", false, "docker compose v2 is required: %v", err)
		return out
	} else if !composeAtLeast(v, 2, 20) {
		add("compose", false, "docker compose %s is too old; 2.20 or newer is required (depends_on required: false)", v)
	} else {
		add("compose", true, "docker compose %s", v)
	}

	inst, err := install.Load(dir)
	if err != nil {
		add("install", false, "%v", err)
		return out
	}
	add("install", true, "%s: profile %s, release %s", inst.Dir, orDash(string(inst.State.Profile)), orDash(inst.State.Version))

	if runtime.GOOS != "windows" {
		if st, err := os.Stat(inst.EnvPath()); err == nil {
			if st.Mode().Perm()&0o077 != 0 {
				add(".env permissions", false, "%s is mode %o; it holds secrets, run: chmod 600 %s", inst.EnvPath(), st.Mode().Perm(), inst.EnvPath())
			} else {
				add(".env permissions", true, "mode %o", st.Mode().Perm())
			}
		}
	}
	if st, err := os.Stat(inst.DataDir()); err != nil || !st.IsDir() {
		add("data directory", false, "%s is missing", inst.DataDir())
	} else {
		add("data directory", true, "%s", inst.DataDir())
	}
	if last := inst.LastUpgrade(); last != nil && last.Status != install.StatusSucceeded && last.Status != install.StatusRolledBack {
		add("last upgrade", false, "upgrade %s is %s (%s); run `opengtm rollback` or finish it", last.ID, last.Status, last.Error)
	}

	p := projectOf(inst, nil)
	if err := p.Config(ctx); err != nil {
		add("compose file", false, "%v", err)
		return out
	}
	add("compose file", true, "valid")

	statuses, err := p.Status(ctx)
	if err != nil {
		add("services", false, "%v", err)
	} else if len(statuses) == 0 {
		add("services", false, "no containers; start them with: docker compose --project-directory %s up -d", inst.Dir)
	} else {
		var bad []string
		for _, s := range statuses {
			switch {
			case s.State == "exited" && s.ExitCode == 0:
			case s.State == "running" && (s.Health == "" || s.Health == "healthy"):
			default:
				bad = append(bad, fmt.Sprintf("%s (%s %s)", s.Service, s.State, s.Health))
			}
		}
		if len(bad) > 0 {
			add("services", false, "not healthy: %s", strings.Join(bad, ", "))
		} else {
			add("services", true, "%d containers running or completed", len(statuses))
		}
	}

	db := composeConn(inst, nil)
	if v, err := db.ServerVersion(ctx); err != nil {
		add("database", false, "%v", err)
	} else {
		add("database", true, "PostgreSQL %s via the %s service", v, "postgres")
		runtimeUser := inst.Env.Value("YUPCHA_RUNTIME_DB_USER")
		if runtimeUser == "" {
			runtimeUser = "yupcha_runtime"
		}
		rows, err := db.Query(ctx, "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = '"+strings.ReplaceAll(runtimeUser, "'", "''")+"'")
		switch {
		case err != nil:
			add("runtime role", false, "%v", err)
		case len(rows) == 0:
			add("runtime role", false, "%s does not exist yet; the migrate service creates it", runtimeUser)
		case rows[0][0] != "f":
			add("runtime role", false, "%s can bypass row-level security", runtimeUser)
		default:
			add("runtime role", true, "%s is NOSUPERUSER NOBYPASSRLS", runtimeUser)
		}
		if revs, err := db.Revisions(ctx); err != nil {
			add("migrations", false, "%v", err)
		} else if len(revs) == 0 {
			add("migrations", false, "the schema was never migrated")
		} else {
			add("migrations", true, "alembic revision %s", strings.Join(revs, ", "))
		}
	}

	if err := upgrade.HTTPHealth(localURL(inst), "")(ctx); err != nil {
		add("front door", false, "%s: %v", localURL(inst), err)
	} else {
		add("front door", true, "%s answers /readyz and proxies the legacy API", localURL(inst))
	}

	list, err := backup.List(inst.BackupDir())
	switch {
	case err != nil:
		add("backups", false, "%v", err)
	case len(list) == 0:
		add("backups", true, "warning: no backups yet; run `opengtm backup --quiesce`")
	default:
		age := time.Since(list[0].Manifest.CreatedAt).Round(time.Hour)
		if age > 7*24*time.Hour {
			add("backups", true, "warning: newest backup is %s old (%s)", age, list[0].Path)
		} else {
			add("backups", true, "%d backups, newest %s old", len(list), age)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func composeAtLeast(v string, major, minor int) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || ma == major && mi >= minor
}
