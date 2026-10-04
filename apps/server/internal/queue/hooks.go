package queue

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
)

// Env carries the dependencies a registrar may need to build handlers.
type Env struct {
	Pool   *pgxpool.Pool
	Config config.Config
	Logger *slog.Logger
}

// Registrar adds handlers for one domain (for example plugin_run).
type Registrar func(env Env, r *Registry) error

var (
	registrarsMu sync.Mutex
	registrars   []namedRegistrar
)

type namedRegistrar struct {
	name string
	fn   Registrar
}

// AddRegistrar lets a domain package contribute handlers from its init(),
// so `opengtm worker` picks them up without importing a central list:
//
//	func init() { queue.AddRegistrar("plugins", registerPluginRun) }
func AddRegistrar(name string, fn Registrar) {
	registrarsMu.Lock()
	defer registrarsMu.Unlock()
	registrars = append(registrars, namedRegistrar{name, fn})
}

// Build runs every globally added registrar, then extra, into a new registry.
func Build(env Env, extra ...Registrar) (*Registry, error) {
	registrarsMu.Lock()
	all := append([]namedRegistrar(nil), registrars...)
	registrarsMu.Unlock()
	for i, fn := range extra {
		all = append(all, namedRegistrar{fmt.Sprintf("extra#%d", i), fn})
	}
	reg := NewRegistry()
	for _, r := range all {
		if err := r.fn(env, reg); err != nil {
			return nil, fmt.Errorf("queue: registrar %s: %w", r.name, err)
		}
	}
	return reg, nil
}

// RunWorker builds the registry and runs a queue until ctx is cancelled.
func RunWorker(ctx context.Context, env Env, extra ...Registrar) error {
	reg, err := Build(env, extra...)
	if err != nil {
		return err
	}
	opts := OptionsFromConfig(env.Config.Worker)
	opts.Logger = env.Logger
	return New(env.Pool, reg, opts).Run(ctx)
}
