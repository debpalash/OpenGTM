package upgrade_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/install"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/upgrade"
)

// fakeDocker answers the docker compose commands the upgrade orchestration
// issues, models one database (current Alembic revision, dump contents) and
// records every step in order. The real Docker/PostgreSQL path is exercised by
// scripts/packaging/e2e.sh and CI; this test pins the decisions: ordering,
// journaling, what a rollback restores and when.
type fakeDocker struct {
	mu       sync.Mutex
	steps    []string
	rev      string   // current alembic revision in the fake database
	dump     string   // contents pg_dump emits
	restored []string // dump contents passed to pg_restore
	released string   // the head the new release's migrations reach

	failPull     bool
	failMigrate  bool // migration errors after changing nothing
	failDump     bool
	failUp       bool
	upFailsOnce  bool
	envAtMigrate string
}

func (f *fakeDocker) step(s string) {
	f.mu.Lock()
	f.steps = append(f.steps, s)
	f.mu.Unlock()
}

func (f *fakeDocker) has(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.steps {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeDocker) index(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.steps {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

func subcommand(args []string) (string, []string) {
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--project-directory", "-f", "-p":
			i += 2
		default:
			return args[i], args[i+1:]
		}
	}
	return "", nil
}

func (f *fakeDocker) Run(_ context.Context, c execx.Cmd) error {
	sub, rest := subcommand(c.Args[1:]) // skip "compose"
	switch sub {
	case "config":
		if len(rest) > 0 && rest[0] == "--services" {
			io.WriteString(w(c), "postgres\nredis\napi\nmigrate\nserver\nworker\n")
		}
		return nil
	case "pull":
		f.step("pull")
		if f.failPull {
			return errors.New("manifest unknown")
		}
		return nil
	case "stop":
		f.step("stop " + strings.Join(rest, ","))
		return nil
	case "up":
		var svcs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && a != "30" && !isNumber(a) {
				svcs = append(svcs, a)
			}
		}
		f.step("up " + strings.Join(svcs, ","))
		if f.failUp && len(svcs) == 0 {
			if f.upFailsOnce {
				f.failUp = false
			}
			return errors.New("container api is unhealthy")
		}
		return nil
	case "run":
		return f.run(c, rest)
	case "exec":
		return f.exec(c, rest)
	}
	return fmt.Errorf("fakeDocker: unhandled %v", c.Args)
}

func w(c execx.Cmd) io.Writer {
	if c.Stdout == nil {
		return io.Discard
	}
	return c.Stdout
}

func isNumber(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func (f *fakeDocker) run(c execx.Cmd, rest []string) error {
	// run --rm --no-deps -T migrate [python -m alembic ...]
	idx := 0
	for idx < len(rest) && strings.HasPrefix(rest[idx], "-") {
		idx++
	}
	cmd := rest[idx+1:] // after the service name
	if len(cmd) >= 4 && cmd[2] == "alembic" {
		switch cmd[3] {
		case "heads":
			io.WriteString(w(c), f.released+" (head)\n")
		case "show":
			if cmd[4] == "rev_v1" || cmd[4] == f.released {
				io.WriteString(w(c), "Rev: "+cmd[4]+"\n")
				return nil
			}
			io.WriteString(w(c), "FAILED: Can't locate revision identified by '"+cmd[4]+"'\n")
			return errors.New("exit status 255")
		}
		return nil
	}
	f.step("migrate")
	if env, err := os.ReadFile(filepath.Join(c.Dir, ".env")); err == nil {
		f.envAtMigrate = string(env)
	}
	if f.failMigrate {
		return errors.New("alembic: relation already exists")
	}
	f.rev = f.released
	return nil
}

func (f *fakeDocker) exec(c execx.Cmd, rest []string) error {
	// exec -T postgres <tool> --username=u --dbname=d ...
	tool := rest[2]
	switch tool {
	case "pg_dump":
		f.step("pg_dump")
		if f.failDump {
			return errors.New("pg_dump: server closed the connection")
		}
		io.WriteString(w(c), f.dump)
		return nil
	case "pg_restore":
		b, _ := io.ReadAll(c.Stdin)
		f.step("pg_restore")
		f.restored = append(f.restored, string(b))
		// Restoring the dump brings back the schema it was taken at.
		f.rev = strings.TrimPrefix(string(b), "DUMP@")
		return nil
	case "psql":
		var sql string
		for _, a := range rest {
			if s, ok := strings.CutPrefix(a, "--command="); ok {
				sql = s
			}
			if a == "--file=-" {
				return nil
			}
		}
		switch {
		case strings.Contains(sql, "DROP SCHEMA"):
			f.step("wipe")
		case strings.Contains(sql, "to_regclass('public.alembic_version')"):
			if f.rev == "" {
				io.WriteString(w(c), "f\n")
			} else {
				io.WriteString(w(c), "t\n")
			}
		case strings.Contains(sql, "SELECT version_num"):
			io.WriteString(w(c), f.rev+"\n")
		case strings.Contains(sql, "server_version"):
			io.WriteString(w(c), "16.4\n")
		case strings.Contains(sql, "relkind IN ('r','p') AND"):
			io.WriteString(w(c), "7\n")
		case strings.Contains(sql, "relkind IN ('r','p','v','m','S','f')"):
			io.WriteString(w(c), "7\n") // not empty: restore must wipe
		}
		return nil
	}
	return fmt.Errorf("fakeDocker: unhandled exec %v", rest)
}

type fixture struct {
	t    *testing.T
	dir  string
	inst *install.Install
	fake *fakeDocker
	out  strings.Builder
	now  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	if _, err := install.Init(install.InitOptions{Dir: dir, Yes: true, Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "workspaces.db"), []byte("v1 data"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst, err := install.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		t: t, dir: dir, inst: inst,
		fake: &fakeDocker{rev: "rev_v1", dump: "DUMP@rev_v1", released: "rev_v2"},
		now:  time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC),
	}
}

func (x *fixture) opts() upgrade.Options {
	return upgrade.Options{
		Install: x.inst, Run: x.fake, Out: &x.out, ToVersion: "2.0.0", NoPull: false,
		StartTimeout: time.Second, BinaryVersion: "test",
		Now: func() time.Time { x.now = x.now.Add(time.Second); return x.now },
	}
}

func (x *fixture) reload() *install.Install {
	x.t.Helper()
	i, err := install.Load(x.dir)
	if err != nil {
		x.t.Fatal(err)
	}
	return i
}

func ctx() context.Context { return context.Background() }

func TestSuccessfulUpgradeOrderingAndJournal(t *testing.T) {
	x := newFixture(t)
	healthChecks := 0
	o := x.opts()
	o.Health = func(context.Context) error { healthChecks++; return nil }

	res, err := upgrade.Upgrade(ctx(), o)
	if err != nil {
		t.Fatalf("%v\n%s", err, x.out.String())
	}

	// Stop applications, then back up, then migrate, then start the stack.
	order := []string{"up postgres", "stop api,migrate,server,worker", "pg_dump", "pull", "migrate"}
	prev := -1
	for _, s := range order {
		i := x.fake.index(s)
		if i < 0 || i <= prev {
			t.Fatalf("step %q out of order or missing (index %d after %d): %v", s, i, prev, x.fake.steps)
		}
		prev = i
	}
	if last := x.fake.steps[len(x.fake.steps)-1]; last != "up " {
		t.Errorf("final step = %q, want a full `up`", last)
	}
	if healthChecks != 1 {
		t.Errorf("health checks = %d", healthChecks)
	}

	// The migration saw the new images: .env was switched before it ran.
	if !strings.Contains(x.fake.envAtMigrate, "OPENGTM_APP_IMAGE=ghcr.io/debpalash/opengtm:2.0.0") {
		t.Errorf("migrate ran with .env:\n%s", x.fake.envAtMigrate)
	}

	inst := x.reload()
	e := inst.LastUpgrade()
	if e == nil || e.Status != install.StatusSucceeded || !e.MigrationStarted || e.Backup == "" {
		t.Fatalf("journal = %+v", e)
	}
	if e.FromRevisions[0] != "rev_v1" || e.ToRevisions[0] != "rev_v2" {
		t.Errorf("revisions %v -> %v", e.FromRevisions, e.ToRevisions)
	}
	if inst.Env.Value("OPENGTM_VERSION") != "2.0.0" || inst.State.Version != "2.0.0" {
		t.Errorf("version not switched: %s / %s", inst.Env.Value("OPENGTM_VERSION"), inst.State.Version)
	}
	if res.BackupPath != e.Backup {
		t.Errorf("result backup = %q", res.BackupPath)
	}
	// The pre-upgrade backup contains the old data directory.
	if _, err := os.Stat(filepath.Join(e.Backup, "data.tar.gz")); err != nil {
		t.Errorf("backup has no data archive: %v", err)
	}
}

func TestNoChangeAndDryRun(t *testing.T) {
	x := newFixture(t)
	o := x.opts()
	o.ToVersion = "1.0.0"
	if _, err := upgrade.Upgrade(ctx(), o); !errors.Is(err, upgrade.ErrNoChange) {
		t.Fatalf("err = %v, want ErrNoChange", err)
	}
	o = x.opts()
	o.DryRun = true
	if _, err := upgrade.Upgrade(ctx(), o); err != nil {
		t.Fatal(err)
	}
	if x.fake.has("pg_dump") || x.fake.has("migrate") || x.fake.has("stop") {
		t.Errorf("dry run changed things: %v", x.fake.steps)
	}
	if got := x.reload().Env.Value("OPENGTM_VERSION"); got != "1.0.0" {
		t.Errorf("dry run edited .env: %s", got)
	}
}

func TestFailedMigrationAutoRollbackRestoresDatabaseDataAndImages(t *testing.T) {
	x := newFixture(t)
	x.fake.failMigrate = true
	o := x.opts()
	o.AutoRollback = true

	_, err := upgrade.Upgrade(ctx(), o)
	if !errors.Is(err, upgrade.ErrRolledBack) {
		t.Fatalf("err = %v, want ErrRolledBack\n%s", err, x.out.String())
	}
	if !x.fake.has("wipe") || len(x.fake.restored) != 1 || x.fake.restored[0] != "DUMP@rev_v1" {
		t.Fatalf("database was not restored from the pre-upgrade dump: steps %v restored %v", x.fake.steps, x.fake.restored)
	}
	inst := x.reload()
	if v := inst.Env.Value("OPENGTM_VERSION"); v != "1.0.0" {
		t.Errorf("version after rollback = %s", v)
	}
	if got := inst.Env.Value("OPENGTM_APP_IMAGE"); got != "ghcr.io/debpalash/opengtm:1.0.0" {
		t.Errorf("app image after rollback = %s", got)
	}
	if e := inst.LastUpgrade(); e.Status != install.StatusRolledBack || e.Error == "" {
		t.Errorf("journal = %+v", e)
	}
	// Data directory is back to the backed-up content.
	if b, _ := os.ReadFile(filepath.Join(x.dir, "data", "workspaces.db")); string(b) != "v1 data" {
		t.Errorf("data after rollback = %q", b)
	}
	if x.fake.rev != "rev_v1" {
		t.Errorf("fake database revision = %s", x.fake.rev)
	}
}

func TestRollbackAfterPullFailureDoesNotTouchTheDatabase(t *testing.T) {
	x := newFixture(t)
	x.fake.failPull = true
	o := x.opts()
	_, err := upgrade.Upgrade(ctx(), o) // no Confirm, no AutoRollback: stays failed
	if err == nil || !strings.Contains(err.Error(), "pull images") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(x.out.String(), "opengtm rollback") {
		t.Errorf("output must tell the operator how to roll back:\n%s", x.out.String())
	}
	inst := x.reload()
	if e := inst.LastUpgrade(); e.Status != install.StatusFailed || e.MigrationStarted {
		t.Fatalf("journal = %+v", e)
	}
	// A new upgrade is refused until the failed one is dealt with? No: failed is
	// terminal, but in_progress is not; this one is failed, so rollback applies.
	x.fake.failPull = false
	ro := x.opts()
	ro.Install = inst
	if _, err := upgrade.Rollback(ctx(), ro); err != nil {
		t.Fatalf("rollback: %v\n%s", err, x.out.String())
	}
	if x.fake.has("pg_restore") || x.fake.has("wipe") {
		t.Errorf("rollback of an upgrade whose migration never ran restored the database: %v", x.fake.steps)
	}
	if got := x.reload().Env.Value("OPENGTM_VERSION"); got != "1.0.0" {
		t.Errorf("version after rollback = %s", got)
	}
	if _, err := upgrade.Rollback(ctx(), ro); err == nil {
		t.Error("second rollback of the same upgrade should be refused")
	}
}

func TestBackupFailureChangesNothingAndRestartsStack(t *testing.T) {
	x := newFixture(t)
	x.fake.failDump = true
	_, err := upgrade.Upgrade(ctx(), x.opts())
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	inst := x.reload()
	if inst.Env.Value("OPENGTM_VERSION") != "1.0.0" || len(inst.State.History) != 0 {
		t.Errorf("state changed: %s %v", inst.Env.Value("OPENGTM_VERSION"), inst.State.History)
	}
	if x.fake.has("migrate") {
		t.Error("migrated without a backup")
	}
	if i := x.fake.index("stop"); i < 0 || x.fake.steps[len(x.fake.steps)-1] != "up " {
		t.Errorf("the stack we stopped was not restarted: %v", x.fake.steps)
	}
	// And no half-written backup is left in backups/.
	if ents, _ := os.ReadDir(filepath.Join(x.dir, "backups")); len(ents) != 0 {
		t.Errorf("partial backup left: %v", ents)
	}
}

func TestDatabaseNewerThanTargetReleaseIsRefusedAndRolledBack(t *testing.T) {
	x := newFixture(t)
	x.fake.rev = "rev_from_the_future" // e.g. someone ran a newer release and now "upgrades" to an older one
	o := x.opts()
	o.AutoRollback = true
	_, err := upgrade.Upgrade(ctx(), o)
	if !errors.Is(err, upgrade.ErrRolledBack) || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v", err)
	}
	if x.fake.has("migrate") {
		t.Error("engine ran against a newer schema")
	}
}

func TestUnhealthyStackOffersRollbackAndConfirmDecides(t *testing.T) {
	x := newFixture(t)
	o := x.opts()
	o.Health = func(context.Context) error { return errors.New("readyz returned 503") }
	o.StartTimeout = 10 * time.Millisecond
	asked := ""
	o.Confirm = func(q string) bool { asked = q; return false }
	_, err := upgrade.Upgrade(ctx(), o)
	if err == nil || !strings.Contains(err.Error(), "health check") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(asked, "Roll back") {
		t.Errorf("operator was not asked: %q", asked)
	}
	if x.fake.has("pg_restore") {
		t.Error("rolled back although the operator said no")
	}
	if e := x.reload().LastUpgrade(); e.Status != install.StatusFailed {
		t.Errorf("status = %s", e.Status)
	}

	// Answering yes later, through `opengtm rollback`, restores the backup.
	ro := x.opts()
	ro.Install = x.reload()
	if _, err := upgrade.Rollback(ctx(), ro); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if len(x.fake.restored) != 1 {
		t.Errorf("restored %v", x.fake.restored)
	}
	// The safety backup of the failed state was taken before the wipe.
	ents, _ := os.ReadDir(filepath.Join(x.dir, "backups"))
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if !strings.Contains(strings.Join(names, " "), "pre-rollback") {
		t.Errorf("no safety backup among %v", names)
	}
}

func TestRollbackOfSuccessfulUpgradeNeedsConfirmation(t *testing.T) {
	x := newFixture(t)
	if _, err := upgrade.Upgrade(ctx(), x.opts()); err != nil {
		t.Fatal(err)
	}
	ro := x.opts()
	ro.Install = x.reload()
	if _, err := upgrade.Rollback(ctx(), ro); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v; a non-interactive rollback of a good upgrade must require --yes", err)
	}
	if x.fake.has("pg_restore") {
		t.Fatal("restored without confirmation")
	}
	ro.Yes = true
	if _, err := upgrade.Rollback(ctx(), ro); err != nil {
		t.Fatal(err)
	}
	if x.fake.rev != "rev_v1" {
		t.Errorf("revision after rollback = %s", x.fake.rev)
	}
}

func TestInterruptedUpgradeBlocksANewOneUntilRolledBack(t *testing.T) {
	x := newFixture(t)
	x.inst.State.History = append(x.inst.State.History, install.Upgrade{ID: "crashed", Status: install.StatusInProgress})
	if err := x.inst.SaveState(); err != nil {
		t.Fatal(err)
	}
	if _, err := upgrade.Upgrade(ctx(), x.opts()); err == nil || !strings.Contains(err.Error(), "never finished") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoBackupUpgradeCannotRestore(t *testing.T) {
	x := newFixture(t)
	x.fake.failMigrate = true
	o := x.opts()
	o.NoBackup = true
	o.AutoRollback = true
	_, err := upgrade.Upgrade(ctx(), o)
	if err == nil || !strings.Contains(err.Error(), "no backup") {
		t.Fatalf("err = %v", err)
	}
	if x.fake.has("pg_restore") {
		t.Error("restored with no backup")
	}
}
