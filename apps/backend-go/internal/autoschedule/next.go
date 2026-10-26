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

// NextRun is compute_next_run: the anchor while now <= anchor, else
// anchor + (floor(elapsed/step)+1)*step. Integer nanosecond arithmetic equals
// Python's float floor at microsecond resolution for every sane elapsed time.
func NextRun(anchor time.Time, interval string, now time.Time) time.Time {
	anchor = anchor.UTC()
	if !now.After(anchor) {
		return anchor
	}
	step := IntervalDelta(interval)
	n := now.Sub(anchor) / step
	return anchor.Add((n + 1) * step)
}

// isoformat renders t like Python's datetime.isoformat() for a UTC-aware
// datetime: microseconds only when non-zero, then "+00:00".
func isoformat(t time.Time) string { return isoformatIn(t, time.UTC) }

// isoformatIn renders t in loc like Python's isoformat for an aware datetime
// whose tzinfo is that zone (psycopg reads timestamptz in the session TimeZone).
func isoformatIn(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + t.Format("-07:00")
}

// FireKey is the schedule fire key, sched:<trigger id>:<next.isoformat()>.
func FireKey(triggerID string, next time.Time) string {
	return "sched:" + triggerID + ":" + isoformat(next)
}

// FireKeyIn is FireKey with the instant rendered in loc, the Postgres session
// TimeZone Python's driver reads timestamptz in.
func FireKeyIn(triggerID string, next time.Time, loc *time.Location) string {
	return "sched:" + triggerID + ":" + isoformatIn(next, loc)
}
