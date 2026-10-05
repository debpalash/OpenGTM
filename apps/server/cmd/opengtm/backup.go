package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/backup"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/install"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/pg"
)

func init() {
	register("backup", "create | verify | list backups of the database, data directory and config", runBackup)
	register("restore", "restore a backup into a database and data directory", runRestore)
}

func runBackup(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "create":
			return backupCreate(ctx, args[1:])
		case "verify":
			return backupVerify(ctx, args[1:])
		case "list":
			return backupList(args[1:])
		case "-h", "--help", "help":
			fmt.Fprintln(os.Stderr, "usage: opengtm backup [create] [flags]\n       opengtm backup verify PATH [--restore-to-scratch-url URL]\n       opengtm backup list [--dir DIR]")
			return flag.ErrHelp
		}
	}
	return backupCreate(ctx, args)
}

func backupCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dir := installFlags(fs)
	dbURL := fs.String("database-url", "", "owner database URL (default: the install's postgres service)")
	out := fs.String("out", "", "directory to write the backup into (default <dir>/backups)")
	label := fs.String("label", "", "short label added to the backup name")
	dataDir := fs.String("data-dir", "", "data directory to include (default: the install's data directory)")
	noData := fs.Bool("no-data", false, "skip the data directory")
	noConfig := fs.Bool("no-config", false, "skip .env, opengtm.yaml and compose.yml (the config holds secrets, including the key that decrypts stored credentials)")
	quiesce := fs.Bool("quiesce", false, "stop the application services during the backup so the database and data directory agree, then restart them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, inst, release, err := connFor(*dir, *dbURL, os.Stderr)
	if err != nil {
		return err
	}
	defer release()

	opts := backup.CreateOptions{
		Dir: *out, Label: *label, Conn: db, DataDir: *dataDir, Version: version,
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}
	if inst != nil {
		if opts.Dir == "" {
			opts.Dir = inst.BackupDir()
		}
		if opts.DataDir == "" && !*noData {
			opts.DataDir = inst.DataDir()
		}
		if !*noConfig {
			opts.ConfigRoot, opts.ConfigFiles = inst.Dir, inst.ConfigFiles()
		}
		opts.Profile = string(inst.State.Profile)
	}
	if *noData {
		opts.DataDir = ""
	}
	if opts.Dir == "" {
		opts.Dir = "backups"
	}

	if *quiesce {
		if inst == nil {
			return errors.New("--quiesce needs an install directory (it stops and restarts the compose services)")
		}
		restart, err := stopApps(ctx, inst)
		if err != nil {
			return err
		}
		defer restart()
	} else if inst != nil {
		fmt.Fprintln(os.Stderr, "note: the stack is left running; the data directory may change while it is archived. Use --quiesce for a strictly consistent backup.")
	}

	path, m, err := backup.Create(ctx, opts)
	if err != nil {
		return err
	}
	if _, err := backup.Verify(path); err != nil {
		return fmt.Errorf("backup was written but failed verification: %w", err)
	}
	var size int64
	for _, f := range m.Files {
		size += f.Size
	}
	fmt.Printf("backup written: %s\n  postgres %s, schema %s, %d tables, %d bytes, files: ", path,
		m.PostgresVersion, strings.Join(m.AlembicRevisions, ","), m.Tables, size)
	var names []string
	for _, f := range m.Files {
		names = append(names, f.Name)
	}
	fmt.Println(strings.Join(names, ", "))
	if len(m.Skipped) > 0 {
		fmt.Printf("  not archived (not regular files): %s\n", strings.Join(m.Skipped, ", "))
	}
	return nil
}

// stopApps stops every service except the data stores and returns a function
// that brings the stack back.
func stopApps(ctx context.Context, inst *install.Install) (restart func(), err error) {
	p := projectOf(inst, os.Stderr)
	services, err := p.Services(ctx)
	if err != nil {
		return nil, err
	}
	var apps []string
	for _, s := range services {
		if s != "postgres" && s != "redis" {
			apps = append(apps, s)
		}
	}
	if err := p.Up(ctx, 3*time.Minute, "postgres"); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "stopping %s\n", strings.Join(apps, ", "))
	if err := p.Stop(ctx, apps...); err != nil {
		return nil, err
	}
	return func() {
		fmt.Fprintln(os.Stderr, "restarting the stack")
		if err := p.Up(context.WithoutCancel(ctx), 5*time.Minute); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not restart the stack:", err)
		}
	}, nil
}

func backupVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup verify", flag.ContinueOnError)
	dir := installFlags(fs)
	scratch := fs.String("restore-to-scratch-url", "", "also restore into this EMPTY scratch database (owner URL) and check the schema revision and table count against the manifest")
	scratchNew := fs.Bool("scratch", false, "also restore into a temporary database created on the server (the install's PostgreSQL, or --server-url), check it against the manifest, and drop it")
	serverURL := fs.String("server-url", "", "owner URL of the PostgreSQL server to create the --scratch database on (default: the install's postgres service)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: opengtm backup verify PATH [--scratch | --restore-to-scratch-url URL]")
	}
	path := pos[0]
	m, err := backup.Verify(path)
	if err != nil {
		return err
	}
	fmt.Printf("checksums ok: %d files, created %s by opengtm %s, schema %s\n",
		len(m.Files), m.CreatedAt.Format(time.RFC3339), m.OpenGTMVersion, strings.Join(m.AlembicRevisions, ","))
	if *scratch == "" && !*scratchNew {
		return nil
	}
	var db pg.Conn
	if *scratchNew {
		server, _, release, err := connFor(*dir, *serverURL, os.Stderr)
		if err != nil {
			return err
		}
		defer release()
		name := fmt.Sprintf("opengtm_verify_%d", time.Now().UnixNano()%1_000_000_000)
		if db, err = server.CreateDatabase(ctx, name); err != nil {
			return fmt.Errorf("create scratch database: %w", err)
		}
		defer func() {
			if err := server.DropDatabase(context.WithoutCancel(ctx), name); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not drop scratch database %s: %v\n", name, err)
			}
		}()
	} else if db, err = pg.Direct(*scratch, execx.OS{}); err != nil {
		return err
	}
	if _, _, err := backup.Restore(ctx, path, backup.RestoreOptions{Conn: db, SkipData: true}); err != nil {
		return fmt.Errorf("restore into scratch database: %w", err)
	}
	revs, err := db.Revisions(ctx)
	if err != nil {
		return err
	}
	tables, err := db.TableCount(ctx)
	if err != nil {
		return err
	}
	if strings.Join(revs, ",") != strings.Join(m.AlembicRevisions, ",") || tables != m.Tables {
		return fmt.Errorf("restored database differs from the manifest: schema %v (want %v), %d tables (want %d)",
			revs, m.AlembicRevisions, tables, m.Tables)
	}
	fmt.Printf("restore into scratch database ok: schema %s, %d tables\n", strings.Join(revs, ","), tables)
	return nil
}

func backupList(args []string) error {
	fs := flag.NewFlagSet("backup list", flag.ContinueOnError)
	dir := installFlags(fs)
	out := fs.String("out", "", "backup directory (default <dir>/backups)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	where := *out
	if where == "" {
		where = *dir + "/backups"
	}
	list, err := backup.List(where)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no backups in", where)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CREATED\tVERSION\tSCHEMA\tSIZE\tPATH")
	for _, e := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", e.Manifest.CreatedAt.Format(time.RFC3339), e.Manifest.OpenGTMVersion,
			strings.Join(e.Manifest.AlembicRevisions, ","), e.Size, e.Path)
	}
	return tw.Flush()
}

func runRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	dir := installFlags(fs)
	dbURL := fs.String("database-url", "", "owner database URL to restore into (default: the install's postgres service)")
	dataDir := fs.String("data-dir", "", "data directory to restore into (default: the install's data directory)")
	wipe := fs.Bool("wipe", false, "replace a non-empty database (drops every schema first). Without it the target must be empty")
	noData := fs.Bool("no-data", false, "restore only the database")
	noRoles := fs.Bool("skip-roles", false, "do not recreate database roles from the backup")
	configDir := fs.String("restore-config", "", "also extract .env, opengtm.yaml and compose.yml into this directory (existing files are kept)")
	yes := fs.Bool("yes", false, "do not ask for confirmation when replacing data")
	start := fs.Bool("start", false, "restart the stack afterwards (Compose installs)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: opengtm restore [flags] BACKUP_DIR")
	}
	path := pos[0]
	m, err := backup.Verify(path)
	if err != nil {
		return err
	}
	db, inst, release, err := connFor(*dir, *dbURL, os.Stderr)
	if err != nil {
		return err
	}
	defer release()

	opts := backup.RestoreOptions{
		Conn: db, DataDir: *dataDir, Wipe: *wipe, SkipRoles: *noRoles, SkipData: *noData, ConfigDir: *configDir,
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}
	if inst != nil && opts.DataDir == "" {
		opts.DataDir = inst.DataDir()
	}

	fmt.Printf("restoring backup %s (created %s, opengtm %s, schema %s)\n  into %s\n",
		path, m.CreatedAt.Format(time.RFC3339), m.OpenGTMVersion, strings.Join(m.AlembicRevisions, ","), db.Describe())
	if *wipe {
		fmt.Println("  --wipe: the target database will be emptied first; data written since this backup is lost")
		if confirm := confirmer(*yes); confirm == nil {
			return errors.New("--wipe replaces data; re-run with --yes to confirm in a non-interactive shell")
		} else if !confirm("Replace the target database?") {
			return errors.New("cancelled")
		}
	}
	if inst != nil {
		if !*wipe {
			// Refuse before stopping anything: a rejected restore must not
			// leave the stack down.
			if err := projectOf(inst, os.Stderr).Up(ctx, 3*time.Minute, "postgres"); err != nil {
				return err
			}
			if empty, err := db.IsEmpty(ctx); err != nil {
				return err
			} else if !empty {
				return fmt.Errorf("%w\nHint: restore into an empty database, or re-run with --wipe", backup.ErrNotEmpty)
			}
		}
		restart, err := stopApps(ctx, inst)
		if err != nil {
			return err
		}
		if *start {
			defer restart()
		} else {
			defer fmt.Fprintf(os.Stderr, "the application services are stopped; start them with: docker compose --project-directory %s up -d\n", inst.Dir)
		}
	}

	_, warnings, err := backup.Restore(ctx, path, opts)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if err != nil {
		if errors.Is(err, backup.ErrNotEmpty) {
			return fmt.Errorf("%w\nHint: restore into an empty database, or re-run with --wipe", err)
		}
		return err
	}
	revs, _ := db.Revisions(ctx)
	fmt.Printf("restore complete: schema %s\n", strings.Join(revs, ","))
	return nil
}
