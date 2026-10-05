package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/install"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/upgrade"
)

func init() {
	register("init", "create .env, opengtm.yaml and compose.yml for a profile (lite, standard, full)", runInit)
}

func runInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := installFlags(fs)
	profile := fs.String("profile", "", "deployment profile: lite, standard or full (prompted when interactive, default lite)")
	yes := fs.Bool("yes", false, "accept defaults and do not prompt")
	force := fs.Bool("force", false, "overwrite existing files (a .bak copy is kept; existing secrets are reused)")
	rotate := fs.Bool("rotate-secrets", false, "generate new secrets even if a .env already has them")
	publicURL := fs.String("public-url", "", "public https URL behind your TLS proxy; switches APP_ENV to production")
	bind := fs.String("bind", "", "host address to publish on (default 127.0.0.1)")
	port := fs.Int("port", 0, "host port (default 3000)")
	project := fs.String("project", "", "Compose project name (default opengtm); use distinct names to run several installs on one host")
	ver := fs.String("version", "", "release to install (default: this binary's version)")
	appImage := fs.String("app-image", "", "Python image reference (overrides the release tag)")
	serverImage := fs.String("server-image", "", "Go server image reference (overrides the release tag)")
	start := fs.Bool("start", false, "run `docker compose up -d --wait` afterwards")
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := install.InitOptions{
		Dir: *dir, Version: *ver, AppImage: *appImage, ServerImage: *serverImage,
		PublicURL: *publicURL, Bind: *bind, Port: *port, Project: *project,
		Yes: *yes, Force: *force, RotateSecrets: *rotate,
		Interactive: isTerminal(os.Stdin), In: os.Stdin, Out: os.Stderr,
	}
	if opts.Version == "" {
		opts.Version = releaseTag()
	}
	if *profile != "" {
		p, err := install.ParseProfile(*profile)
		if err != nil {
			return err
		}
		opts.Profile = p
	}
	res, err := install.Init(opts)
	if err != nil {
		return err
	}

	fmt.Printf("Initialised %s (profile %s)\n", res.Dir, res.Profile)
	for _, f := range res.BackedUp {
		fmt.Printf("  kept previous copy: %s\n", f)
	}
	for _, f := range res.Written {
		fmt.Printf("  wrote %s\n", f)
	}
	for _, w := range res.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
	if res.CredentialsFile != "" {
		fmt.Printf("\nAdmin sign-in is in %s (mode 0600). Change the password after signing in and delete the file.\n", res.CredentialsFile)
	}

	if !*start {
		fmt.Printf("\nNext: cd %s && docker compose up -d --wait\nThen open %s\n", res.Dir, res.URL)
		return nil
	}
	inst, release, err := loadInstall(res.Dir)
	if err != nil {
		return err
	}
	defer release()
	p := projectOf(inst, os.Stderr)
	fmt.Println("\nStarting the stack (the first start pulls images and migrates the database)...")
	if err := p.Config(ctx); err != nil {
		return fmt.Errorf("compose file is invalid: %w", err)
	}
	if err := p.Up(ctx, 10*time.Minute); err != nil {
		return fmt.Errorf("start: %w\nInspect with: docker compose --project-directory %s ps", err, res.Dir)
	}
	check := upgrade.HTTPHealth(localURL(inst), "")
	if err := waitFor(ctx, check, 2*time.Minute); err != nil {
		return fmt.Errorf("the stack started but is not serving yet: %w", err)
	}
	fmt.Printf("OpenGTM is running at %s\n", res.URL)
	return nil
}

// localURL is where the front door answers from this host.
func localURL(inst *install.Install) string {
	host := inst.Env.Value("OPENGTM_BIND")
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	port := inst.Env.Value("OPENGTM_PORT")
	if port == "" {
		port = "3000"
	}
	return "http://" + host + ":" + port
}

func waitFor(ctx context.Context, check upgrade.HealthFunc, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		if last = check(ctx); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-time.After(2 * time.Second):
		}
	}
}
