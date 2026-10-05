package pluginrun

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/kernels"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// Version is embedded in the egress User-Agent; the binary sets it at startup.
var Version = "dev"

func init() { queue.AddRegistrar("plugins", register) }

// register builds the plugin runtime once per worker process: the catalog,
// one shared egress client and the compiled extraction kernel.
func register(env queue.Env, r *queue.Registry) error {
	catalog := LoadCatalog(env.Config.Plugins)
	LogCatalog(env.Logger, catalog)
	client, err := egress.New(egress.Options{Version: Version, ProxyURL: env.Config.Plugins.EgressProxy})
	if err != nil {
		return fmt.Errorf("plugins: egress client: %w", err)
	}
	k, err := kernels.Load(context.Background())
	if err != nil {
		return fmt.Errorf("plugins: load extraction kernel: %w", err)
	}
	runner := NewRunner(client, k, env.Logger)
	if catalog.HasRuntime("process") {
		// Start the supervisor now so a crashed predecessor's plugin
		// processes are reclaimed at boot and the worker is hardened before
		// the first plugin runs. A failure here is logged, not fatal: runs of
		// process plugins fail permanently with the reason, everything else
		// keeps working.
		if _, err := runner.ProcessSupervisor(); err != nil {
			env.Logger.Warn("process plugins are disabled", "err", err)
		}
	}
	NewWorker(env.Pool, catalog, runner, env.Logger).Register(r)
	return nil
}

// LogCatalog reports what was installed and what was rejected.
func LogCatalog(log *slog.Logger, c *Catalog) {
	if log == nil {
		log = slog.Default()
	}
	names := make([]string, 0, len(c.byName))
	for _, e := range c.List() {
		names = append(names, e.Plugin.Name)
	}
	log.Info("plugins loaded", "count", len(names), "plugins", names)
	for _, e := range c.Errors {
		log.Warn("plugin rejected", "path", e.Path, "error", e.Error)
	}
}
