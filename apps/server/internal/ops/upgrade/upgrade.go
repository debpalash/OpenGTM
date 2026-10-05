// Package upgrade moves an OpenGTM install to another release and back.
//
// Upgrade: back up (with the apps stopped, so the database and data directory
// agree), journal the intent, switch the image tags in .env, run the single
// migration owner from the new image, start everything, and check health. If
// anything after the .env switch fails, the operator is offered a rollback.
//
// Rollback restores the pre-upgrade backup. It does not run Alembic
// downgrades: those are not guaranteed to preserve data, and a restore is
// something CI can prove. The cost is that anything written after the backup
// is lost, which is why a rollback first takes a safety backup of the current
// state.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/backup"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/compose"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/install"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/migrate"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/pg"
)

// ErrNoChange means the install already runs the requested images.
var ErrNoChange = errors.New("already running the requested release")

// ErrRolledBack wraps the failure of an upgrade that was rolled back.
var ErrRolledBack = errors.New("upgrade failed and was rolled back")

// HealthFunc reports whether the stack is serving. It is polled until it
// returns nil or the timeout passes.
type HealthFunc func(ctx context.Context) error

// Options configures Upgrade and Rollback.
type Options struct {
	Install *install.Install
	Run     execx.Runner
	Out     io.Writer

	// Upgrade only.
	ToVersion   string // release tag, with or without a leading v
	AppImage    string // optional full image reference overrides
	ServerImage string
	NoBackup    bool
	NoPull      bool
	Force       bool // proceed even if the images are unchanged
	DryRun      bool
	// AutoRollback rolls back without asking when the upgrade fails.
	AutoRollback bool
	// Confirm asks the operator a yes/no question. Nil means "no" (a
	// non-interactive run never rolls back or proceeds by itself).
	Confirm func(question string) bool

	// Rollback only.
	Entry          *install.Upgrade // nil: the latest unfinished or successful entry
	NoSafetyBackup bool
	// Yes confirms rolling back an upgrade that had succeeded.
	Yes bool

	BinaryVersion string // recorded in backups
	// Health builds the check for a release: it is given the release version
	// the stack should report ("" or "latest" when unknown). An upgrade checks
	// the new release; a rollback checks the one it returns to.
	Health func(expectVersion string) HealthFunc
	// StartTimeout bounds `docker compose up --wait`. Default 5 minutes.
	StartTimeout time.Duration
	Now          func() time.Time
}

func (o *Options) out() io.Writer {
	if o.Out == nil {
		return io.Discard
	}
	return o.Out
}

func (o *Options) logf(format string, args ...any) {
	fmt.Fprintf(o.out(), format+"\n", args...)
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Options) timeout() time.Duration {
	if o.StartTimeout > 0 {
		return o.StartTimeout
	}
	return 5 * time.Minute
}

func (o *Options) project() compose.Project {
	return compose.Project{Dir: o.Install.Dir, Name: o.Install.ProjectName(), Run: o.Run, Log: o.out()}
}

func (o *Options) conn(p compose.Project) pg.Conn {
	env := o.Install.Env
	user, db := env.Value("POSTGRES_USER"), env.Value("POSTGRES_DB")
	if user == "" {
		user = "yupcha"
	}
	if db == "" {
		db = "yupcha"
	}
	return pg.Conn{Compose: &p, Service: "postgres", User: user, Database: db, Run: o.Run}
}

// appServices are the services stopped around a backup or restore: everything
// except the data stores.
func appServices(all []string) []string {
	var out []string
	for _, s := range all {
		if s != "postgres" && s != "redis" {
			out = append(out, s)
		}
	}
	return out
}

// Result summarises an upgrade or rollback.
type Result struct {
	Entry      install.Upgrade
	BackupPath string
}

var tagRepo = regexp.MustCompile(`^(.*?)(?::[^:/]+)?$`)

// retag replaces the tag of image (keeping its repository).
func retag(image, tag string) string {
	m := tagRepo.FindStringSubmatch(image)
	repo := image
	if m != nil {
		repo = m[1]
	}
	return repo + ":" + tag
}

func setImages(env *install.EnvFile, version, app, server string) {
	env.Set("OPENGTM_VERSION", version)
	env.Set("OPENGTM_APP_IMAGE", app)
	env.Set("OPENGTM_SERVER_IMAGE", server)
}

// Upgrade runs the upgrade. The caller holds the install lock.
func Upgrade(ctx context.Context, o Options) (res *Result, err error) {
	inst := o.Install
	version := strings.TrimPrefix(o.ToVersion, "v")
	if version == "" && o.AppImage == "" {
		return nil, errors.New("upgrade: no target release; pass --to <version> (or --app-image and --server-image)")
	}
	curApp, curServer := inst.Env.Value("OPENGTM_APP_IMAGE"), inst.Env.Value("OPENGTM_SERVER_IMAGE")
	curVersion := inst.Env.Value("OPENGTM_VERSION")
	newApp, newServer := o.AppImage, o.ServerImage
	if newApp == "" {
		newApp = retag(curApp, version)
	}
	if newServer == "" {
		newServer = retag(curServer, version)
	}
	if version == "" {
		version = imageTag(newApp)
	}
	if newApp == curApp && newServer == curServer && !o.Force {
		return nil, fmt.Errorf("%w (%s)", ErrNoChange, curApp)
	}
	if last := inst.LastUpgrade(); last != nil && last.Status == install.StatusInProgress {
		return nil, fmt.Errorf("upgrade: a previous upgrade (%s) never finished; run `opengtm rollback` to restore the pre-upgrade state, "+
			"or remove its journal entry in %s once you have checked the install by hand", last.ID, inst.StatePath())
	}

	p := o.project()
	db := o.conn(p)
	o.logf("upgrading %s: %s -> %s", inst.Dir, curVersion, version)
	o.logf("  app    %s -> %s", curApp, newApp)
	o.logf("  server %s -> %s", curServer, newServer)

	if err := p.Config(ctx); err != nil {
		return nil, fmt.Errorf("upgrade: compose file is invalid: %w", err)
	}
	services, err := p.Services(ctx)
	if err != nil {
		return nil, err
	}
	if o.DryRun {
		o.logf("dry run: would stop %s, back up to %s, switch images, run migrate, start the stack and check health",
			strings.Join(appServices(services), ", "), inst.BackupDir())
		return &Result{}, nil
	}

	o.logf("starting PostgreSQL")
	if err := p.Up(ctx, o.timeout(), "postgres"); err != nil {
		return nil, fmt.Errorf("upgrade: PostgreSQL did not become healthy: %w", err)
	}
	fromRevs, err := db.Revisions(ctx)
	if err != nil {
		return nil, fmt.Errorf("upgrade: read current schema revision: %w", err)
	}

	// Anything that fails before the .env switch leaves the install as it was,
	// so the only recovery needed is to restart what we stopped.
	apps := appServices(services)
	restart := func() {
		if rerr := p.Up(context.WithoutCancel(ctx), o.timeout()); rerr != nil {
			o.logf("warning: could not restart the stack: %v", rerr)
		}
	}
	if len(apps) > 0 {
		o.logf("stopping application services so the backup is consistent: %s", strings.Join(apps, ", "))
		if err := p.Stop(ctx, apps...); err != nil {
			return nil, err
		}
	}

	entry := install.Upgrade{
		ID: o.now().UTC().Format("20060102T150405Z"), StartedAt: o.now().UTC(), Status: install.StatusInProgress,
		FromVersion: curVersion, ToVersion: version,
		FromAppImage: curApp, FromServerImage: curServer, ToAppImage: newApp, ToServerImage: newServer,
		FromRevisions: fromRevs,
	}
	if o.NoBackup {
		o.logf("warning: --no-backup given; rollback will not be able to restore data")
	} else {
		o.logf("backing up before the upgrade")
		path, m, berr := backup.Create(ctx, backup.CreateOptions{
			Dir: inst.BackupDir(), Label: "pre-upgrade-" + curVersion + "-to-" + version,
			Conn: db, DataDir: inst.DataDir(), ConfigRoot: inst.Dir, ConfigFiles: inst.ConfigFiles(),
			Version: o.BinaryVersion, Profile: string(inst.State.Profile), Now: o.now,
			Logf: func(f string, a ...any) { o.logf("  "+f, a...) },
		})
		if berr != nil {
			restart()
			return nil, fmt.Errorf("upgrade: pre-upgrade backup failed, nothing was changed: %w", berr)
		}
		if _, verr := backup.Verify(path); verr != nil {
			restart()
			return nil, fmt.Errorf("upgrade: pre-upgrade backup failed verification, nothing was changed: %w", verr)
		}
		entry.Backup = path
		o.logf("backup verified: %s (schema %s)", path, strings.Join(m.AlembicRevisions, ","))
	}

	inst.State.History = append(inst.State.History, entry)
	if err := inst.SaveState(); err != nil {
		restart()
		return nil, err
	}
	cur := &inst.State.History[len(inst.State.History)-1]
	save := func() { _ = inst.SaveState() }

	// From here on the install has changed; failures offer a rollback.
	fail := func(cause error) (*Result, error) {
		cur.Status, cur.Error, cur.FinishedAt = install.StatusFailed, cause.Error(), o.now().UTC()
		save()
		o.logf("upgrade failed: %v", cause)
		return o.offerRollback(ctx, cur, cause)
	}

	// New releases may need a newer compose.yml (services, dependencies); the
	// one embedded in this binary is the one the release was tested with.
	refreshed, err := inst.RefreshCompose(entry.ID)
	if err != nil {
		return fail(fmt.Errorf("refresh compose.yml: %w", err))
	}
	switch refreshed.Action {
	case install.RefreshUpdated:
		cur.ComposeBackup = refreshed.Previous
		o.logf("compose.yml updated to the version shipped with this opengtm (previous copy: %s)", refreshed.Previous)
	case install.RefreshCustomized:
		o.logf("warning: compose.yml has local changes and was left as is. The version this release expects is %s; merge it before relying on new services", refreshed.Proposed)
	}

	setImages(inst.Env, version, newApp, newServer)
	if err := inst.Env.Save(); err != nil {
		return fail(err)
	}
	inst.State.Version, inst.State.AppImage, inst.State.ServerImage = version, newApp, newServer
	save()

	if !o.NoPull {
		o.logf("pulling images")
		if err := p.Pull(ctx); err != nil {
			return fail(fmt.Errorf("pull images (use --no-pull for locally built images): %w", err))
		}
	}

	cur.MigrationStarted = true
	save()
	mres, err := migrate.Apply(ctx, migrate.Options{DB: db, Engine: migrate.ComposeEngine(p), Out: o.out()})
	if err != nil {
		return fail(err)
	}
	cur.ToRevisions = mres.After.Current

	o.logf("starting the stack")
	if err := p.Up(ctx, o.timeout()); err != nil {
		return fail(fmt.Errorf("start services: %w", err))
	}
	if err := o.waitHealthy(ctx, version); err != nil {
		return fail(fmt.Errorf("health check: %w", err))
	}

	cur.Status, cur.FinishedAt = install.StatusSucceeded, o.now().UTC()
	save()
	o.logf("upgrade complete: %s now runs %s (schema %s)", inst.Dir, version, strings.Join(cur.ToRevisions, ","))
	if cur.Backup != "" {
		o.logf("rollback is available with `opengtm rollback` until you take the next upgrade (backup %s)", cur.Backup)
	}
	return &Result{Entry: *cur, BackupPath: cur.Backup}, nil
}

func imageTag(image string) string {
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return "latest"
}

func (o *Options) offerRollback(ctx context.Context, e *install.Upgrade, cause error) (*Result, error) {
	if e.Backup == "" {
		return nil, fmt.Errorf("%w; no backup was taken, so there is nothing to roll back to", cause)
	}
	do := o.AutoRollback
	if !do && o.Confirm != nil {
		do = o.Confirm("Roll back to the pre-upgrade backup now?")
	}
	if !do {
		o.logf("the install is in a failed state. Roll back with:\n  opengtm rollback --dir %s", o.Install.Dir)
		return nil, cause
	}
	rb := *o
	rb.Entry = e
	rb.NoSafetyBackup = true // the failed upgrade's own backup is the safe point
	if _, rerr := Rollback(ctx, rb); rerr != nil {
		return nil, fmt.Errorf("%w; rollback also failed: %v", cause, rerr)
	}
	return nil, fmt.Errorf("%w: %v", ErrRolledBack, cause)
}

func (o *Options) waitHealthy(ctx context.Context, expectVersion string) error {
	if o.Health == nil {
		return nil
	}
	check := o.Health(expectVersion)
	ctx, cancel := context.WithTimeout(ctx, o.timeout())
	defer cancel()
	var last error
	for {
		if last = check(ctx); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not healthy after %s: %w", o.timeout(), last)
		case <-time.After(2 * time.Second):
		}
	}
}

// Rollback restores the install to the state recorded before an upgrade.
func Rollback(ctx context.Context, o Options) (*Result, error) {
	inst := o.Install
	var entry *install.Upgrade
	if o.Entry != nil {
		entry = o.Entry
	} else {
		for i := len(inst.State.History) - 1; i >= 0; i-- {
			if h := &inst.State.History[i]; h.Status != install.StatusRolledBack {
				entry = h
				break
			}
		}
	}
	if entry == nil {
		return nil, errors.New("rollback: no upgrade to roll back (nothing recorded in .opengtm/state.json)")
	}
	if entry.Status == install.StatusRolledBack {
		return nil, fmt.Errorf("rollback: upgrade %s was already rolled back", entry.ID)
	}
	// Re-resolve the pointer into State.History so state updates persist.
	for i := range inst.State.History {
		if inst.State.History[i].ID == entry.ID {
			entry = &inst.State.History[i]
		}
	}
	restoreDB := entry.MigrationStarted || entry.Status == install.StatusSucceeded
	if restoreDB && entry.Backup == "" {
		return nil, errors.New("rollback: this upgrade ran without a backup (--no-backup), so the database and data cannot be restored. " +
			"Restore your own backup with `opengtm restore`, or set the image lines in .env back by hand")
	}
	if entry.Status == install.StatusSucceeded && !o.Yes && o.Confirm != nil {
		q := fmt.Sprintf("Upgrade %s succeeded. Rolling back discards everything written since %s. Continue?",
			entry.ID, entry.StartedAt.Format(time.RFC3339))
		if !o.Confirm(q) {
			return nil, errors.New("rollback: cancelled")
		}
	} else if entry.Status == install.StatusSucceeded && !o.Yes && o.Confirm == nil {
		return nil, errors.New("rollback: the upgrade succeeded; rolling back discards data written since the backup. Re-run with --yes to confirm")
	}

	p := o.project()
	db := o.conn(p)
	if restoreDB {
		o.logf("verifying backup %s", entry.Backup)
		if _, err := backup.Verify(entry.Backup); err != nil {
			return nil, fmt.Errorf("rollback: %w (nothing was changed)", err)
		}
	}

	o.logf("rolling back %s -> %s", entry.ToVersion, entry.FromVersion)
	if err := p.Up(ctx, o.timeout(), "postgres"); err != nil {
		return nil, fmt.Errorf("rollback: PostgreSQL did not become healthy: %w", err)
	}
	services, err := p.Services(ctx)
	if err != nil {
		return nil, err
	}
	if apps := appServices(services); len(apps) > 0 {
		o.logf("stopping application services: %s", strings.Join(apps, ", "))
		if err := p.Stop(ctx, apps...); err != nil {
			return nil, err
		}
	}

	if restoreDB {
		if !o.NoSafetyBackup {
			o.logf("taking a safety backup of the current state first")
			path, _, err := backup.Create(ctx, backup.CreateOptions{
				Dir: inst.BackupDir(), Label: "pre-rollback", Conn: db, DataDir: inst.DataDir(),
				ConfigRoot: inst.Dir, ConfigFiles: inst.ConfigFiles(), Version: o.BinaryVersion,
				Profile: string(inst.State.Profile), Now: o.now,
			})
			if err != nil {
				return nil, fmt.Errorf("rollback: safety backup failed (pass --no-safety-backup if the database is unusable): %w", err)
			}
			o.logf("  safety backup: %s", path)
		}
		o.logf("restoring database and data directory from %s", entry.Backup)
		_, warns, err := backup.Restore(ctx, entry.Backup, backup.RestoreOptions{
			Conn: db, DataDir: inst.DataDir(), Wipe: true, Now: o.now,
			Logf: func(f string, a ...any) { o.logf("  "+f, a...) },
		})
		if err != nil {
			return nil, fmt.Errorf("rollback: restore failed: %w", err)
		}
		for _, w := range warns {
			o.logf("  note: %s", w)
		}
	} else {
		o.logf("the migration never started, so the database is unchanged and is not restored")
	}

	if entry.ComposeBackup != "" {
		if err := inst.RestoreCompose(entry.ComposeBackup); err != nil {
			return nil, fmt.Errorf("rollback: restore compose.yml: %w", err)
		}
	}
	setImages(inst.Env, entry.FromVersion, entry.FromAppImage, entry.FromServerImage)
	if err := inst.Env.Save(); err != nil {
		return nil, err
	}
	inst.State.Version, inst.State.AppImage, inst.State.ServerImage = entry.FromVersion, entry.FromAppImage, entry.FromServerImage
	entry.Status, entry.FinishedAt = install.StatusRolledBack, o.now().UTC()
	if err := inst.SaveState(); err != nil {
		return nil, err
	}

	o.logf("starting the previous release")
	if err := p.Up(ctx, o.timeout()); err != nil {
		return nil, fmt.Errorf("rollback: the previous release did not start: %w", err)
	}
	if err := o.waitHealthy(ctx, entry.FromVersion); err != nil {
		return nil, fmt.Errorf("rollback: %w", err)
	}
	o.logf("rollback complete: %s runs %s again", inst.Dir, entry.FromVersion)
	return &Result{Entry: *entry, BackupPath: entry.Backup}, nil
}

// HTTPHealth polls the front door: /readyz (database reachable), /health
// proxied to the Python API, and, when expectVersion is a concrete release,
// that the running server reports it.
func HTTPHealth(baseURL, expectVersion string) HealthFunc {
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(ctx context.Context, path string) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%s returned %s", path, resp.Status)
		}
		return string(b), nil
	}
	return func(ctx context.Context) error {
		if _, err := get(ctx, "/readyz"); err != nil {
			return err
		}
		if _, err := get(ctx, "/health"); err != nil {
			return fmt.Errorf("legacy API through the front door: %w", err)
		}
		if expectVersion != "" && expectVersion != "latest" {
			body, err := get(ctx, "/api/v2/version")
			if err != nil {
				return err
			}
			if !strings.Contains(body, `"`+expectVersion+`"`) {
				return fmt.Errorf("server reports %s, expected version %s", strings.TrimSpace(body), expectVersion)
			}
		}
		return nil
	}
}

// BackupDirOf is a small helper for callers that print where backups go.
func BackupDirOf(i *install.Install) string { return filepath.Clean(i.BackupDir()) }
