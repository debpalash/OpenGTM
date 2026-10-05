package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/compose"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
)

// SQLAlchemyURL converts a postgresql:// URL to the psycopg form Alembic reads.
func SQLAlchemyURL(u string) string {
	if rest, ok := strings.CutPrefix(u, "postgresql://"); ok {
		return "postgresql+psycopg://" + rest
	}
	return u
}

// FindRoot returns the first of candidates that contains the Alembic setup
// (alembic.ini and apps/api/scripts/migrate.py).
func FindRoot(candidates ...string) (string, bool) {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, "alembic.ini")); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, "apps", "api", "scripts", "migrate.py")); err != nil {
			continue
		}
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		return abs, true
	}
	return "", false
}

// LocalEngine runs Alembic from a checkout or the Python image's /app, using
// the interpreter command in Python (for example ["python"] or
// ["uv","run","--frozen","python"]).
func LocalEngine(root string, python []string, ownerURL string, run execx.Runner) CommandEngine {
	if run == nil {
		run = execx.OS{}
	}
	if len(python) == 0 {
		python = []string{"python"}
	}
	env := []string{
		"DATABASE_URL=" + SQLAlchemyURL(ownerURL),
		"PYTHONPATH=" + root,
		"YUPCHA_DB_INIT=alembic",
	}
	cmd := func(args ...string) execx.Cmd {
		return execx.Cmd{Name: python[0], Args: append(append([]string{}, python[1:]...), args...), Dir: root, Env: env}
	}
	return CommandEngine{
		Label: "local python (" + root + ")",
		Alembic: func(ctx context.Context, args ...string) (string, error) {
			return execx.Output(ctx, run, cmd(append([]string{"-m", "alembic"}, args...)...))
		},
		Migrate: func(ctx context.Context) error {
			return execx.Do(ctx, run, cmd("-m", "apps.api.scripts.migrate"))
		},
	}
}

// ComposeEngine runs Alembic in the install's one-shot `migrate` service,
// which is the release's own Python image: it always carries exactly the
// migrations that belong to the images being deployed.
func ComposeEngine(p compose.Project) CommandEngine {
	return CommandEngine{
		Label: "compose service migrate",
		Alembic: func(ctx context.Context, args ...string) (string, error) {
			return p.RunOnceOutput(ctx, "migrate", append([]string{"python", "-m", "alembic"}, args...)...)
		},
		Migrate: func(ctx context.Context) error {
			return p.RunOnce(ctx, "migrate")
		},
	}
}
