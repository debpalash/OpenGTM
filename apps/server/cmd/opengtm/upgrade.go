package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/upgrade"
)

func init() {
	register("upgrade", "back up, migrate and move an install to another release, with health checks and rollback", runUpgrade)
	register("rollback", "restore the pre-upgrade backup and the previous release", runRollback)
}

func runUpgrade(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	dir := installFlags(fs)
	to := fs.String("to", "", "release to upgrade to (default: this binary's version)")
	appImage := fs.String("app-image", "", "Python image reference (overrides --to)")
	serverImage := fs.String("server-image", "", "Go server image reference (overrides --to)")
	noBackup := fs.Bool("no-backup", false, "skip the pre-upgrade backup (rollback then cannot restore data)")
	noPull := fs.Bool("no-pull", false, "do not pull images (locally built or pre-loaded images)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	auto := fs.Bool("auto-rollback", false, "roll back automatically if the upgrade fails after it started changing things")
	dry := fs.Bool("dry-run", false, "print the plan and change nothing")
	force := fs.Bool("force", false, "run even if the images are already the requested release")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the stack to become healthy")
	if err := fs.Parse(args); err != nil {
		return err
	}
	inst, release, err := loadInstall(*dir)
	if err != nil {
		return err
	}
	defer release()

	target := *to
	if target == "" && *appImage == "" {
		target = releaseTag()
		if target == "" {
			return errors.New("this is a development build with no release version; pass --to <version> (or --app-image and --server-image)")
		}
	}
	o := upgrade.Options{
		Install: inst, Run: execx.OS{}, Out: os.Stderr, ToVersion: target,
		AppImage: *appImage, ServerImage: *serverImage,
		NoBackup: *noBackup, NoPull: *noPull, Force: *force, DryRun: *dry, AutoRollback: *auto,
		// --yes skips the "proceed?" question below but is not consent to a
		// rollback, which discards data: that needs a person or --auto-rollback.
		Confirm: confirmer(false), BinaryVersion: version, StartTimeout: *timeout,
	}
	if target == "" {
		o.ToVersion = tagOf(*serverImage)
	}
	o.Health = func(expect string) upgrade.HealthFunc { return upgrade.HTTPHealth(localURL(inst), expect) }

	if !*dry && !*yes {
		proceed := confirmer(false)
		if proceed == nil {
			return errors.New("upgrade stops the stack and migrates the database; re-run with --yes in a non-interactive shell (or --dry-run to see the plan)")
		}
		if !proceed("Stop the stack, back up, migrate and upgrade?") {
			return errors.New("cancelled")
		}
	}
	res, err := upgrade.Upgrade(ctx, o)
	if errors.Is(err, upgrade.ErrNoChange) {
		fmt.Println(err)
		return nil
	}
	if err != nil {
		return err
	}
	if !*dry {
		fmt.Printf("upgraded to %s\n", res.Entry.ToVersion)
	}
	return nil
}

func runRollback(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	dir := installFlags(fs)
	id := fs.String("id", "", "upgrade id to roll back (default: the latest not yet rolled back; see .opengtm/state.json)")
	yes := fs.Bool("yes", false, "confirm discarding data written since the backup")
	noSafety := fs.Bool("no-safety-backup", false, "skip the backup of the current state taken before restoring (only if the database is unusable)")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the stack to become healthy")
	if err := fs.Parse(args); err != nil {
		return err
	}
	inst, release, err := loadInstall(*dir)
	if err != nil {
		return err
	}
	defer release()

	o := upgrade.Options{
		Install: inst, Run: execx.OS{}, Out: os.Stderr, Yes: *yes, NoSafetyBackup: *noSafety,
		Confirm: confirmer(*yes), BinaryVersion: version, StartTimeout: *timeout,
	}
	if *id != "" {
		for i := range inst.State.History {
			if inst.State.History[i].ID == *id {
				o.Entry = &inst.State.History[i]
			}
		}
		if o.Entry == nil {
			return fmt.Errorf("no upgrade with id %q in %s", *id, inst.StatePath())
		}
	}
	o.Health = func(expect string) upgrade.HealthFunc { return upgrade.HTTPHealth(localURL(inst), expect) }
	res, err := upgrade.Rollback(ctx, o)
	if err != nil {
		return err
	}
	fmt.Printf("rolled back to %s\n", res.Entry.FromVersion)
	return nil
}

func tagOf(image string) string {
	for i := len(image) - 1; i >= 0; i-- {
		switch image[i] {
		case ':':
			return image[i+1:]
		case '/':
			return ""
		}
	}
	return ""
}
