package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/backup"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/install"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/optest"
)

func TestParseInterspersedAcceptsFlagsAfterPositionals(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	wipe := fs.Bool("wipe", false, "")
	dir := fs.String("dir", "", "")
	pos, err := parseInterspersed(fs, []string{"BACKUP", "--wipe", "--dir", "/srv/og"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 1 || pos[0] != "BACKUP" || !*wipe || *dir != "/srv/og" {
		t.Fatalf("pos=%v wipe=%v dir=%q", pos, *wipe, *dir)
	}
}

func TestComposeAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"2.20.0": true, "2.39.1": true, "v2.20.3": true, "5.5.1": true,
		"2.19.9": false, "1.29.2": false, "": false, "garbage": false,
	} {
		if got := composeAtLeast(v, 2, 20); got != want {
			t.Errorf("composeAtLeast(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestReleaseTagAndLocalURL(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = "dev"
	if releaseTag() != "" {
		t.Error("a development build has no release tag")
	}
	version = "v3.1.0"
	if releaseTag() != "3.1.0" {
		t.Errorf("releaseTag = %q", releaseTag())
	}

	dir := t.TempDir()
	if _, err := install.Init(install.InitOptions{Dir: dir, Yes: true, Version: "3.1.0", Bind: "0.0.0.0", Port: 4321}); err != nil {
		t.Fatal(err)
	}
	inst, err := install.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := localURL(inst); got != "http://127.0.0.1:4321" {
		t.Errorf("a wildcard bind is probed on loopback, got %s", got)
	}
}

func TestInitCommandWritesInstallAndRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--dir", dir, "--yes", "--profile", "standard", "--port", "41234", "--project", "cli-test", "--version", "9.8.7"}
	if err := runInit(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	inst, err := install.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Env.Value("COMPOSE_PROFILES") != "standard" || inst.Env.Value("COMPOSE_PROJECT_NAME") != "cli-test" ||
		inst.Env.Value("OPENGTM_APP_IMAGE") != "ghcr.io/debpalash/opengtm:9.8.7" {
		t.Errorf("env keys = %v", inst.Env.Keys())
	}
	before, _ := os.ReadFile(filepath.Join(dir, ".env"))
	err = runInit(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "already exist") {
		t.Fatalf("second init: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, ".env")); string(after) != string(before) {
		t.Error("second init changed .env")
	}
	if err := runInit(context.Background(), []string{"--dir", t.TempDir(), "--yes", "--profile", "huge"}); err == nil {
		t.Error("unknown profile accepted")
	}
}

// The URL mode of the commands (no Compose install): backup, verify with a
// scratch restore, and restore, driven through the real command functions.
func TestBackupVerifyRestoreCommandsAgainstURLs(t *testing.T) {
	server := optest.Server(t)
	src := optest.Scratch(t, server, "cli_src")
	ctx := context.Background()
	if err := src.Exec(ctx, "CREATE TABLE alembic_version (version_num varchar(32)); INSERT INTO alembic_version VALUES ('cli000000001'); CREATE TABLE t (v text); INSERT INTO t VALUES ('hello')"); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := runBackup(ctx, []string{"--database-url", src.URL, "--out", out, "--no-data", "--no-config", "--label", "cli"}); err != nil {
		t.Fatal(err)
	}
	list, err := backup.List(out)
	if err != nil || len(list) != 1 {
		t.Fatalf("backups: %v %v", list, err)
	}
	path := list[0].Path

	scratch := optest.Scratch(t, server, "cli_verify")
	if err := runBackup(ctx, []string{"verify", path, "--restore-to-scratch-url", scratch.URL}); err != nil {
		t.Fatalf("verify with scratch restore: %v", err)
	}

	dst := optest.Scratch(t, server, "cli_dst")
	if err := runRestore(ctx, []string{path, "--database-url", dst.URL, "--no-data"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	rows, err := dst.Query(ctx, "SELECT v FROM t")
	if err != nil || len(rows) != 1 || rows[0][0] != "hello" {
		t.Fatalf("restored rows %v %v", rows, err)
	}
	// A second restore into the now populated database is refused...
	if err := runRestore(ctx, []string{path, "--database-url", dst.URL, "--no-data"}); err == nil || !strings.Contains(err.Error(), "--wipe") {
		t.Fatalf("restore into a populated database: %v", err)
	}
	// ...and --wipe without --yes needs confirmation, which a test has no terminal for.
	if err := runRestore(ctx, []string{path, "--database-url", dst.URL, "--no-data", "--wipe"}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("--wipe without --yes: %v", err)
	}
	if err := runRestore(ctx, []string{path, "--database-url", dst.URL, "--no-data", "--wipe", "--yes"}); err != nil {
		t.Fatalf("wipe restore: %v", err)
	}
}
