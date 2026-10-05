package migrate_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/migrate"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/optest"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/pg"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return c
}

// fakeEngine models an Alembic graph a -> b -> c with head c, and records calls.
type fakeEngine struct {
	db       pg.Conn
	t        *testing.T
	upgrades []string
	noop     bool // pretend to migrate without changing the database
}

var order = []string{"a", "b", "c"}

func (f *fakeEngine) Describe() string                        { return "fake" }
func (f *fakeEngine) Heads(context.Context) ([]string, error) { return []string{"c"}, nil }
func (f *fakeEngine) Known(_ context.Context, r string) (bool, error) {
	for _, o := range order {
		if o == r {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeEngine) Upgrade(ctx context.Context, target string) error {
	f.upgrades = append(f.upgrades, target)
	if f.noop {
		return nil
	}
	want := "c"
	if target != "head" {
		want = target
	}
	return f.db.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS alembic_version (version_num varchar(32) NOT NULL);
DELETE FROM alembic_version; INSERT INTO alembic_version VALUES ('%s')`, want))
}

func setup(t *testing.T) (*fakeEngine, pg.Conn) {
	t.Helper()
	server := optest.Server(t)
	db := optest.Scratch(t, server, "mig")
	return &fakeEngine{db: db, t: t}, db
}

func TestApplyFreshThenNoop(t *testing.T) {
	eng, db := setup(t)
	var out bytes.Buffer
	res, err := migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng, Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !res.Applied || !res.Before.Fresh() || !res.After.UpToDate() {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(out.String(), "unmigrated") {
		t.Errorf("output should say the database was unmigrated:\n%s", out.String())
	}

	res, err = migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng})
	if err != nil || res.Applied {
		t.Fatalf("second run: applied=%v err=%v; an up-to-date schema must not invoke the engine", res.Applied, err)
	}
	if len(eng.upgrades) != 1 {
		t.Fatalf("engine invoked %d times", len(eng.upgrades))
	}
}

func TestPlanChangesNothing(t *testing.T) {
	eng, db := setup(t)
	if _, err := migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng, Plan: true}); err != nil {
		t.Fatal(err)
	}
	if len(eng.upgrades) != 0 {
		t.Fatal("plan mode ran the engine")
	}
	if revs, _ := db.Revisions(ctx(t)); revs != nil {
		t.Fatalf("plan mode changed the database: %v", revs)
	}
}

func TestUpgradeFromOlderRevision(t *testing.T) {
	eng, db := setup(t)
	if _, err := migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng, Target: "a"}); err != nil {
		t.Fatal(err)
	}
	res, err := migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng})
	if err != nil || !res.Applied || res.Before.Current[0] != "a" || res.After.Current[0] != "c" {
		t.Fatalf("upgrade a -> head: %+v %v", res, err)
	}
}

func TestRefusesDatabaseNewerThanRelease(t *testing.T) {
	eng, db := setup(t)
	if err := db.Exec(ctx(t), "CREATE TABLE alembic_version (version_num varchar(32)); INSERT INTO alembic_version VALUES ('from_the_future')"); err != nil {
		t.Fatal(err)
	}
	_, err := migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng})
	if !errors.Is(err, migrate.ErrNewerDatabase) {
		t.Fatalf("err = %v, want ErrNewerDatabase", err)
	}
	if len(eng.upgrades) != 0 {
		t.Fatal("engine ran against a database from a newer release")
	}
	if !strings.Contains(err.Error(), "from_the_future") || !strings.Contains(err.Error(), "restore") {
		t.Errorf("error should name the revision and the remedy: %v", err)
	}
}

func TestVerificationCatchesEngineThatDidNothing(t *testing.T) {
	eng, db := setup(t)
	eng.noop = true
	if err := db.Exec(ctx(t), "CREATE TABLE alembic_version (version_num varchar(32)); INSERT INTO alembic_version VALUES ('a')"); err != nil {
		t.Fatal(err)
	}
	_, err := migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng})
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("err = %v, want verification failure", err)
	}
}

func TestLockIsExclusiveAndReleased(t *testing.T) {
	server := optest.Server(t)
	db := optest.Scratch(t, server, "lock")
	release, err := migrate.Lock(ctx(t), db.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Lock(ctx(t), db.URL, 600*time.Millisecond); err == nil || !strings.Contains(err.Error(), "another migration") {
		t.Fatalf("second owner err = %v", err)
	}
	release()
	release2, err := migrate.Lock(ctx(t), db.URL, time.Second)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	release2()
}

func TestParseHeads(t *testing.T) {
	out := "INFO  [alembic.runtime.migration] Context impl PostgresqlImpl.\nf9a0b1c2d3e4 (head)\n7c1e5a9d3b20 (head)\n"
	got := migrate.ParseHeads(out)
	if len(got) != 2 || got[0] != "f9a0b1c2d3e4" || got[1] != "7c1e5a9d3b20" {
		t.Fatalf("ParseHeads = %v", got)
	}
}

// TestRealAlembic drives the real migration graph through the local engine:
// partial upgrade to an older revision (an "installed previous release"),
// then the owner upgrade to head, then the unknown-revision guard.
func TestRealAlembic(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping real Alembic test")
	}
	_, file, _, _ := runtime.Caller(0)
	root, ok := migrate.FindRoot(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", ".."))
	if !ok {
		t.Skip("repository root with alembic.ini not found")
	}
	server := optest.Server(t)
	db := optest.Scratch(t, server, "alembic")
	eng := migrate.LocalEngine(root, []string{"uv", "run", "--frozen", "python"}, db.URL, nil)

	heads, err := eng.Heads(ctx(t))
	if err != nil || len(heads) != 1 {
		t.Fatalf("heads = %v, %v", heads, err)
	}
	if known, err := eng.Known(ctx(t), "does_not_exist"); err != nil || known {
		t.Fatalf("Known(garbage) = %v, %v", known, err)
	}
	if known, err := eng.Known(ctx(t), heads[0]); err != nil || !known {
		t.Fatalf("Known(head) = %v, %v", known, err)
	}

	hist, err := eng.Alembic(ctx(t), "history")
	if err != nil {
		t.Fatal(err)
	}
	rx := regexp.MustCompile(`(?m)^\S+ -> ([0-9a-f]+)`)
	all := rx.FindAllStringSubmatch(hist, -1)
	if len(all) < 12 {
		t.Fatalf("expected a long history, got %d entries", len(all))
	}
	older := all[10][1] // an earlier release's schema

	if _, err := migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng, Target: older}); err != nil {
		t.Fatalf("install older schema: %v", err)
	}
	var out bytes.Buffer
	res, err := migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng, Out: &out})
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out.String())
	}
	if !res.Applied || !res.After.UpToDate() || res.Before.Current[0] != older {
		t.Fatalf("upgrade result = %+v", res)
	}

	// A database stamped by a newer release is refused without running Alembic.
	if err := db.Exec(ctx(t), "UPDATE alembic_version SET version_num = 'ffffffffffff'"); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(ctx(t), migrate.Options{DB: db, Engine: eng}); !errors.Is(err, migrate.ErrNewerDatabase) {
		t.Fatalf("newer-database guard: %v", err)
	}
}
