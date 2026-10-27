package schedboot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Subsystem is one bootstrap: Run returns what Python's bootstrap_* returned (a count or a bool),
// logged per pass.
type Subsystem struct {
	Name string
	Run  func(ctx context.Context) (any, error)
}

const (
	defaultInterval = 60 * time.Second
	minInterval     = 10 * time.Second
	// DefaultSubsystemTimeout bounds one bootstrap, so a stuck query cannot starve the others.
	DefaultSubsystemTimeout = 5 * time.Minute
)

// ReconcileInterval is max(10, int(SCHEDULER_RECONCILE_SECONDS or 60)) seconds; an unparsable value
// falls back to the default (Python would refuse to start).
func ReconcileInterval(get func(string) string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(get("SCHEDULER_RECONCILE_SECONDS")))
	if err != nil {
		return defaultInterval
	}
	return max(minInterval, time.Duration(n)*time.Second)
}

// ReconcileOnce runs every subsystem in order. A failing, panicking or hung subsystem is reported as
// "error: ..." under its name and never stops the ones after it (reconcile_once).
func ReconcileOnce(ctx context.Context, subs []Subsystem, log *slog.Logger, timeout time.Duration) map[string]any {
	return reconcile(ctx, subs, log, timeout, nil)
}

// reconcile is ReconcileOnce with a lease guard: still is asked before every subsystem after the first and the
// pass stops (the remaining subsystems do not run) as soon as it reports the lease lost, so a connection that
// died mid-pass cannot leave two reconcilers working at once.
func reconcile(ctx context.Context, subs []Subsystem, log *slog.Logger, timeout time.Duration, still func(context.Context) bool) map[string]any {
	if timeout <= 0 {
		timeout = DefaultSubsystemTimeout
	}
	res := make(map[string]any, len(subs))
	for i, s := range subs {
		if i > 0 && still != nil && !still(ctx) {
			log.Warn("scheduler lost the lease mid-pass; stopping the pass", "next", s.Name)
			break
		}
		res[s.Name] = runOne(ctx, s, log, timeout)
	}
	return res
}

func runOne(ctx context.Context, s Subsystem, log *slog.Logger, timeout time.Duration) (out any) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("scheduler reconciliation panicked", "subsystem", s.Name, "panic", fmt.Sprint(r))
			out = fmt.Sprintf("error: panic: %v", r)
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := s.Run(cctx)
	if err != nil {
		log.Error("scheduler reconciliation failed", "subsystem", s.Name, "err", err.Error())
		return "error: " + err.Error()
	}
	return v
}

// RunConfig configures Run. Hold reports whether this instance is the active scheduler on this pass
// (Lease.Hold); a standby idles without touching the database.
type RunConfig struct {
	Interval         time.Duration
	SubsystemTimeout time.Duration
	Log              *slog.Logger
	Subsystems       []Subsystem
	Hold             func(ctx context.Context) (bool, error)
}

// Run reconciles every Interval while this instance holds the lease, until ctx is cancelled.
func Run(ctx context.Context, c RunConfig) error {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	interval := max(c.Interval, time.Millisecond)
	log.Info("scheduler ready", "interval", interval.String())
	wasLeader := false
	for ctx.Err() == nil {
		leader, err := true, error(nil)
		if c.Hold != nil {
			leader, err = c.Hold(ctx)
		}
		switch {
		case err != nil:
			if ctx.Err() == nil {
				log.Error("scheduler lease check failed", "err", err.Error())
			}
		case leader:
			if !wasLeader {
				log.Info("scheduler is active (lease held)")
			}
			var still func(context.Context) bool
			if c.Hold != nil {
				still = func(ctx context.Context) bool {
					ok, err := c.Hold(ctx)
					return err == nil && ok
				}
			}
			log.Info("scheduler reconciliation", "results", reconcile(ctx, c.Subsystems, log, c.SubsystemTimeout, still))
		case wasLeader:
			log.Warn("scheduler lost the lease; standing by")
		}
		wasLeader = err == nil && leader
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
	log.Info("scheduler stopped")
	return nil
}
