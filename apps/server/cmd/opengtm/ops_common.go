package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/compose"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/install"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/pg"
)

// Helpers shared by the operator commands (init, migrate, backup, restore,
// upgrade, rollback, doctor --dir).

// installFlags adds --dir, the install directory every operator command works on.
func installFlags(fs *flag.FlagSet) *string {
	return fs.String("dir", ".", "install directory (the one containing .env and compose.yml)")
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// confirmer returns a yes/no prompt reading stdin, or nil when stdin is not a
// terminal (a scripted run never answers questions on its own).
func confirmer(yes bool) func(string) bool {
	if yes {
		return func(string) bool { return true }
	}
	if !isTerminal(os.Stdin) {
		return nil
	}
	in := bufio.NewReader(os.Stdin)
	return func(q string) bool {
		fmt.Fprintf(os.Stderr, "%s [y/N]: ", q)
		line, _ := in.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		return line == "y" || line == "yes"
	}
}

// loadInstall loads the install in dir and takes its operation lock.
func loadInstall(dir string) (*install.Install, func(), error) {
	inst, err := install.Load(dir)
	if err != nil {
		return nil, nil, err
	}
	release, err := inst.Lock()
	if err != nil {
		return nil, nil, err
	}
	return inst, release, nil
}

func projectOf(inst *install.Install, out io.Writer) compose.Project {
	return compose.Project{Dir: inst.Dir, Name: inst.ProjectName(), Run: execx.OS{}, Log: out}
}

// composeConn returns a connection that runs PostgreSQL tools inside the
// install's postgres container.
func composeConn(inst *install.Install, out io.Writer) pg.Conn {
	p := projectOf(inst, out)
	user, db := inst.Env.Value("POSTGRES_USER"), inst.Env.Value("POSTGRES_DB")
	if user == "" {
		user = "yupcha"
	}
	if db == "" {
		db = "yupcha"
	}
	return pg.Conn{Compose: &p, Service: "postgres", User: user, Database: db, Run: execx.OS{}}
}

// explicitURL returns the owner database URL given by flag or by
// OPENGTM_ADMIN_DATABASE_URL. Owner rights are required: pg_dump as the
// runtime role cannot read RLS-protected tables, and migrations need DDL.
func explicitURL(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("OPENGTM_ADMIN_DATABASE_URL")
}

// connFor resolves how to reach the database. An explicit URL wins; otherwise
// the Compose install in dir (PostgreSQL tools run inside its container);
// otherwise DATABASE_URL from the environment. inst is nil in URL mode.
func connFor(dir, dbURL string, out io.Writer) (pg.Conn, *install.Install, func(), error) {
	direct := func(u string) (pg.Conn, *install.Install, func(), error) {
		c, err := pg.Direct(u, execx.OS{})
		return c, nil, func() {}, err
	}
	if u := explicitURL(dbURL); u != "" {
		return direct(u)
	}
	if installExists(dir) {
		inst, release, err := loadInstall(dir)
		if err != nil {
			return pg.Conn{}, nil, nil, err
		}
		return composeConn(inst, out), inst, release, nil
	}
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return direct(u)
	}
	return pg.Conn{}, nil, nil, fmt.Errorf("%s is not an opengtm install (run `opengtm init`), and no database was given: "+
		"pass --database-url (owner role) or set OPENGTM_ADMIN_DATABASE_URL", dir)
}

func installExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".env"))
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, compose.File))
	return err == nil
}

// releaseTag turns the build version into an image tag; source builds ("dev")
// have no release to upgrade to by default.
func releaseTag() string {
	if version == "" || version == "dev" {
		return ""
	}
	return strings.TrimPrefix(version, "v")
}

// parseInterspersed parses fs allowing flags before and after positional
// arguments (`opengtm restore BACKUP --wipe`), which flag.Parse alone rejects.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
