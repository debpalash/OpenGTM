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

// NextRunIn is compute_next_run when the anchor carries the Postgres session
// zone loc (psycopg returns timestamptz as ZoneInfo-aware datetimes). The
// elapsed time is absolute (now is a UTC datetime), but Python's
// anchor + n*delta is wall-clock arithmetic: the local time of day is kept and
// the offset recomputed, so a daily 07:00 rule stays at 07:00 across DST. A
// local time that falls in a gap or overlap resolves with fold=0 (the offset
// before the transition).
func NextRunIn(anchor time.Time, interval string, now time.Time, loc *time.Location) time.Time {
	anchor = anchor.UTC()
	if !now.After(anchor) {
		return anchor
	}
	step := IntervalDelta(interval)
	n := now.Sub(anchor)/step + 1
	if loc == nil || loc == time.UTC {
		return anchor.Add(n * step)
	}
	return wallClockAdd(anchor, n*step, loc)
}

// wallClockAdd is aware-datetime + timedelta in Python: add d to the local
// wall-clock fields of t in loc and re-resolve the offset with fold=0.
func wallClockAdd(t time.Time, d time.Duration, loc *time.Location) time.Time {
	lt := t.In(loc)
	_, off := lt.Zone()
	// Local wall clock as a naive instant (UTC fields = local fields).
	w := lt.Add(time.Duration(off) * time.Second).UTC().Add(d)
	wall := w.Unix()
	before := offsetAt(loc, wall-86400-int64(off)) // offset a day earlier
	after := offsetAt(loc, wall+86400-int64(off))  // offset a day later
	cand := func(o int) (time.Time, bool) {
		u := time.Unix(wall-int64(o), int64(w.Nanosecond())).UTC()
		return u, offsetAt(loc, u.Unix()) == o
	}
	if u, ok := cand(before); ok { // unambiguous, or overlap: first occurrence
		return u
	}
	if u, ok := cand(after); ok {
		return u
	}
	u, _ := cand(before) // gap: pre-transition offset
	return u
}

func offsetAt(loc *time.Location, unix int64) int {
	_, off := time.Unix(unix, 0).In(loc).Zone()
	return off
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
