package enrich

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/pluginrun"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// DomainRPSEnv overrides DefaultDomainRPS.
const DomainRPSEnv = "OPENGTM_ENRICH_DOMAIN_RPS"

func init() {
	queue.AddRegistrar("enrich", register)
	// Available in Go but not claimed until `opengtm routes set
	// run_workbook_connector go`; the migration seeds no route.
	queue.DeclareSwitchable(JobType)
}

func secondsDuration(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func register(env queue.Env, r *queue.Registry) error {
	rps := DefaultDomainRPS
	if v := os.Getenv(DomainRPSEnv); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
			return fmt.Errorf("%s must be a positive number, got %q", DomainRPSEnv, v)
		}
		rps = f
	}
	client, err := egress.New(egress.Options{
		Version: pluginrun.Version, ProxyURL: env.Config.Plugins.EgressProxy, DefaultRPS: rps,
	})
	if err != nil {
		return fmt.Errorf("enrich: egress client: %w", err)
	}
	catalog := pluginrun.LoadCatalog(env.Config.Plugins)
	NewWorker(env.Pool, catalog, client, env.Logger, rps).Register(r)
	return nil
}
