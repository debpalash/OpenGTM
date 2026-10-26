package leadjobs

import (
	"context"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

type panicProber struct{}

func (panicProber) SourceList(context.Context) ([]string, error) { panic("probe must not run") }
func (panicProber) SourceProbe(context.Context, []string, time.Duration) (map[string]*int, error) {
	panic("probe must not run")
}

// SOURCE_HEALTH_ENABLED off: no list, no probe, no database access (a nil pool would panic) and
// no reschedule, so toggling the flag off drains the chain.
func TestFlagOffNoopNoReschedule(t *testing.T) {
	for _, v := range []string{"", "0", "false", "off"} {
		d := Deps{Py: panicProber{}, Getenv: envOf(map[string]string{"SOURCE_HEALTH_ENABLED": v})}
		if err := HandleSourceHealthCheck(context.Background(), d, &queue.Claim{ID: 1, Type: JobType}); err != nil {
			t.Fatalf("%q: %v", v, err)
		}
	}
}

func TestNextRunAtCadence(t *testing.T) {
	now := time.Date(2026, 3, 4, 10, 30, 0, 123, time.UTC)
	for iv, want := range map[string]time.Time{
		"daily":  time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC),
		"hourly": time.Date(2026, 3, 4, 11, 0, 0, 0, time.UTC),
		"weekly": time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC), // anchor Thursday 2026-01-01 + 9 weeks
		"":       time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC),
	} {
		if got := nextRunAt(iv, now); !got.Equal(want) {
			t.Errorf("%q: %v, want %v", iv, got, want)
		}
	}
}

func TestFireKeyShape(t *testing.T) {
	got := FireKey(time.Date(2026, 3, 5, 0, 0, 0, 999, time.UTC))
	if got != "source_health:2026-03-05T00:00:00+00:00" {
		t.Fatal(got)
	}
}

func TestKnobsDefaultsAndClamp(t *testing.T) {
	k := knobsFrom(envOf(nil))
	if k.concurrency != 8 || k.outageRatio != 0.6 || k.interval != "daily" {
		t.Fatalf("%+v", k)
	}
	k = knobsFrom(envOf(map[string]string{"SOURCE_HEALTH_PROBE_CONCURRENCY": "0", "SOURCE_HEALTH_OUTAGE_RATIO": "0.5", "SOURCE_HEALTH_INTERVAL": "hourly"}))
	if k.concurrency != 1 || k.outageRatio != 0.5 || k.interval != "hourly" {
		t.Fatalf("%+v", k)
	}
}
