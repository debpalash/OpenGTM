// Package migrate is the single owner of schema migrations.
//
// Decision (see docs/self-hosting.md and the RFC): the migration engine stays
// Alembic until the Go API owns the schema, because every revision is Python
// code and re-implementing 60+ of them in Go would add risk with no benefit.
// What moves to Go now is everything around the engine that makes upgrades
// safe: one serialized owner (advisory lock and install-directory lock), an
// explicit plan, a refusal to run against a database that is newer than the
// code, post-condition verification against the Alembic head, and the choice
// of where the engine runs (the Python release image through Compose, a local
// Python environment, or a custom command).
//
// When the Go API takes over the schema, only the Executor changes.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/pg"
)

// Engine runs Alembic somewhere. Implementations must use the owner role.
type Engine interface {
	// Heads returns the head revision(s) of the release's migration graph.
	Heads(ctx context.Context) ([]string, error)
	// Known reports whether the release's migration graph contains rev.
	Known(ctx context.Context, rev string) (bool, error)
	// Upgrade applies migrations up to target ("head" for the latest) and then
	// provisions the runtime login role.
	Upgrade(ctx context.Context, target string) error
	// Describe names the engine for logs.
	Describe() string
}

// Status compares the database with a release.
type Status struct {
	Current []string // revisions in alembic_version; nil for an unmigrated database
	Heads   []string
	// Unknown lists current revisions the release does not contain: the
	// database is newer than (or foreign to) this release.
	Unknown []string
}

// UpToDate reports whether the database is at the release head.
func (s Status) UpToDate() bool {
	return len(s.Unknown) == 0 && len(s.Current) > 0 && sameSet(s.Current, s.Heads)
}

// Fresh reports an unmigrated database.
func (s Status) Fresh() bool { return len(s.Current) == 0 }

// ErrNewerDatabase means the database was migrated by a newer release.
var ErrNewerDatabase = errors.New("database schema is newer than this release")

// Inspect reads the database revision and compares it with the engine's graph.
func Inspect(ctx context.Context, db pg.Conn, e Engine) (Status, error) {
	var s Status
	var err error
	if s.Current, err = db.Revisions(ctx); err != nil {
		return s, fmt.Errorf("migrate: read alembic_version: %w", err)
	}
	if s.Heads, err = e.Heads(ctx); err != nil {
		return s, fmt.Errorf("migrate: read release heads: %w", err)
	}
	for _, rev := range s.Current {
		ok, err := e.Known(ctx, rev)
		if err != nil {
			return s, fmt.Errorf("migrate: look up revision %s: %w", rev, err)
		}
		if !ok {
			s.Unknown = append(s.Unknown, rev)
		}
	}
	return s, nil
}

// Options configures Apply.
type Options struct {
	DB     pg.Conn
	Engine Engine
	// Target is a revision or "head" (default).
	Target string
	// Plan inspects and reports without changing anything.
	Plan bool
	Out  io.Writer
}

// Result is the outcome of Apply.
type Result struct {
	Before, After Status
	Applied       bool
}

// Apply is the migration owner entry point: inspect, refuse unsafe states,
// run the engine, verify.
func Apply(ctx context.Context, o Options) (Result, error) {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	target := o.Target
	if target == "" {
		target = "head"
	}
	var res Result
	var err error
	if res.Before, err = Inspect(ctx, o.DB, o.Engine); err != nil {
		return res, err
	}
	res.After = res.Before
	b := res.Before
	fmt.Fprintf(out, "database %s: %s\n", o.DB.Describe(), describeRevs(b.Current))
	fmt.Fprintf(out, "release head: %s (engine: %s)\n", describeRevs(b.Heads), o.Engine.Describe())

	if len(b.Unknown) > 0 {
		return res, fmt.Errorf("%w: revision %s is not part of this release. Running an older release "+
			"against a newer schema is unsupported and Alembic downgrades are not guaranteed to preserve data; "+
			"upgrade the release, or restore a backup taken before the newer migration (opengtm rollback / restore)",
			ErrNewerDatabase, strings.Join(b.Unknown, ", "))
	}
	if target == "head" && b.UpToDate() {
		fmt.Fprintln(out, "schema is up to date; nothing to do")
		return res, nil
	}
	if o.Plan {
		if b.Fresh() {
			fmt.Fprintln(out, "plan: create the schema and apply every migration up to "+target)
		} else {
			fmt.Fprintln(out, "plan: apply pending migrations up to "+target)
		}
		return res, nil
	}

	fmt.Fprintf(out, "applying migrations to %s\n", target)
	if err := o.Engine.Upgrade(ctx, target); err != nil {
		return res, fmt.Errorf("migrate: engine failed (on PostgreSQL Alembic normally applies the whole upgrade in one "+
			"transaction, so the schema is usually unchanged; check with `opengtm migrate --plan`): %w", err)
	}
	res.Applied = true
	if res.After, err = Inspect(ctx, o.DB, o.Engine); err != nil {
		return res, err
	}
	want := res.After.Heads
	if target != "head" {
		want = []string{target}
	}
	if !sameSet(res.After.Current, want) {
		return res, fmt.Errorf("migrate: verification failed: database is at %s, expected %s",
			describeRevs(res.After.Current), describeRevs(want))
	}
	fmt.Fprintf(out, "schema now at %s\n", describeRevs(res.After.Current))
	return res, nil
}

func describeRevs(r []string) string {
	if len(r) == 0 {
		return "no revision (unmigrated)"
	}
	return strings.Join(r, ", ")
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

var headLine = regexp.MustCompile(`(?m)^([0-9A-Za-z_]+)\s+\(head\)`)

// ParseHeads extracts revision ids from `alembic heads` output.
func ParseHeads(out string) []string {
	var heads []string
	for _, m := range headLine.FindAllStringSubmatch(out, -1) {
		heads = append(heads, m[1])
	}
	return heads
}

// CommandEngine runs Alembic through commands. Run executes an argument
// vector in the engine's environment (a local checkout, or a one-shot
// container of the release image) and returns stdout.
type CommandEngine struct {
	Label string
	// Alembic runs `python -m alembic <args>` in the engine environment.
	Alembic func(ctx context.Context, args ...string) (string, error)
	// Migrate runs the release's full migration entrypoint (schema upgrade to
	// head plus runtime role provisioning).
	Migrate func(ctx context.Context) error
}

// Describe implements Engine.
func (c CommandEngine) Describe() string { return c.Label }

// Heads implements Engine.
func (c CommandEngine) Heads(ctx context.Context) ([]string, error) {
	out, err := c.Alembic(ctx, "heads")
	if err != nil {
		return nil, err
	}
	heads := ParseHeads(out)
	if len(heads) == 0 {
		return nil, fmt.Errorf("could not parse `alembic heads` output: %q", out)
	}
	return heads, nil
}

// Known implements Engine. `alembic show` exits non-zero for an unknown
// revision; that is the one failure we translate, anything else is returned.
func (c CommandEngine) Known(ctx context.Context, rev string) (bool, error) {
	out, err := c.Alembic(ctx, "show", rev)
	if err == nil {
		return true, nil
	}
	// Alembic prints "FAILED: Can't locate revision ..." on stdout.
	var xe *execx.Error
	if errors.As(err, &xe) && strings.Contains(out+xe.Stderr, "Can't locate revision") {
		return false, nil
	}
	return false, err
}

// Upgrade implements Engine.
func (c CommandEngine) Upgrade(ctx context.Context, target string) error {
	if target == "head" {
		return c.Migrate(ctx)
	}
	// A partial upgrade is an operator tool (staging a rollout, testing the
	// upgrade path from an older schema); it skips role provisioning.
	_, err := c.Alembic(ctx, "upgrade", target)
	return err
}
