// Package autoschedule ports the schedule half of automations/engine.py:
// wall-clock aligned next-run math, the non-RLS scheduled_triggers mirror and
// the single-flight trigger_eval enqueue. The Python scheduler stays the only
// ticker; the bootstrap functions here enqueue byte-identical rows.
package autoschedule

import (
	"fmt"
	"time"
)

// IntervalDelta is _interval_delta: hourly, daily, weekly; anything else daily.
func IntervalDelta(interval string) time.Duration {
	switch interval {
	case "hourly":
		return time.Hour
	case "weekly":
		return 7 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// NextRun is compute_next_run for a UTC session: the anchor while now <= anchor,
// else anchor + (floor(elapsed/step)+1)*step. Integer nanosecond arithmetic
// equals Python's float floor at microsecond resolution for every sane elapsed
// time.
func NextRun(anchor time.Time, interval string, now time.Time) time.Time {
	return NextRunIn(anchor, interval, now, time.UTC)
}

// NextRunIn is NextRun with a legacy zone argument that is ignored. Python's
// compute_next_run added the interval to a session-zone-aware datetime
// (wall-clock arithmetic, so a daily rule shifted by an hour across DST and
// fire keys depended on the Postgres TimeZone). Audit F1: the next run is
// always anchor + n*interval on absolute instants, identical under every
// session TimeZone.
func NextRunIn(anchor time.Time, interval string, now time.Time, _ *time.Location) time.Time {
	anchor = anchor.UTC()
	if !now.After(anchor) {
		return anchor
	}
	step := IntervalDelta(interval)
	n := now.Sub(anchor)/step + 1
	return anchor.Add(n * step)
}

// isoformat renders t like Python's datetime.isoformat() for a UTC-aware
// datetime: microseconds only when non-zero, then "+00:00".
func isoformat(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "+00:00"
}

// FireKey is the schedule fire key, sched:<trigger id>:<next.isoformat()>.
func FireKey(triggerID string, next time.Time) string {
	return "sched:" + triggerID + ":" + isoformat(next)
}

// FireKeyIn is FireKey; the zone argument is ignored. Fire keys render in UTC
// whatever the Postgres session TimeZone (audit F1).
func FireKeyIn(triggerID string, next time.Time, _ *time.Location) string {
	return FireKey(triggerID, next)
}
