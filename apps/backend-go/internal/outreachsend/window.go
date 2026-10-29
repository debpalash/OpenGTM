package outreachsend

import (
	"time"
	_ "time/tzdata" // IANA names resolve without a system zoneinfo, like Python's tzdata fallback

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
)

// legacyZones are the pre-2024b abbreviation zones the Python reference
// environment does not resolve (its ZoneInfo raises, so the window falls back to
// UTC). Go would resolve them; the vectors pin Python's answer. README T26.
var legacyZones = map[string]bool{"EST": true, "MST": true, "HST": true, "EST5EDT": true,
	"CST6CDT": true, "MST7MDT": true, "PST8PDT": true}

// windowLoc is ZoneInfo(name) with Python's fallbacks: empty or invalid names
// (including path-like and "Local") are UTC.
func windowLoc(name *string) *time.Location {
	if name == nil || *name == "" || *name == "Local" || legacyZones[*name] {
		return time.UTC
	}
	loc, err := time.LoadLocation(*name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// InSendWindow is _in_send_window: no window (nil bounds or start == end) is
// always open; start < end is a same-day window; otherwise it wraps midnight.
func InSendWindow(seq outreachstore.Seq, now time.Time) bool {
	if seq.SendWindowStart == nil || seq.SendWindowEnd == nil || *seq.SendWindowStart == *seq.SendWindowEnd {
		return true
	}
	start, end := *seq.SendWindowStart, *seq.SendWindowEnd
	h := now.In(windowLoc(seq.SendWindowTZ)).Hour()
	if start < end {
		return start <= h && h < end
	}
	return h >= start || h < end
}

// NextWindowSlot is _next_window_slot: the next top-of-hour start in the
// sequence's zone that is strictly after now (wall-clock comparison, like Python's
// same-tzinfo aware compare), as UTC.
func NextWindowSlot(seq outreachstore.Seq, now time.Time) time.Time {
	start := 0
	if seq.SendWindowStart != nil {
		start = *seq.SendWindowStart
	}
	loc := windowLoc(seq.SendWindowTZ)
	local := now.In(loc)
	y, m, d := local.Date()
	sod := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute +
		time.Duration(local.Second())*time.Second + time.Duration(local.Nanosecond())
	if time.Duration(start)*time.Hour <= sod {
		d++
	}
	return wallToUTC(y, m, d, start, loc)
}

// wallToUTC resolves a wall-clock time like Python's aware datetime with fold=0:
// an ambiguous time takes its first occurrence, a nonexistent one (spring-forward
// gap) takes the offset in force before the transition.
func wallToUTC(y int, m time.Month, d, hour int, loc *time.Location) time.Time {
	wall := time.Date(y, m, d, hour, 0, 0, 0, time.UTC)
	offAt := func(t time.Time) int { _, o := t.In(loc).Zone(); return o }
	offs := map[int]bool{}
	for _, dh := range []int{-36, -12, 12, 36} {
		offs[offAt(wall.Add(time.Duration(dh)*time.Hour))] = true
	}
	var best time.Time
	found := false
	for o := range offs {
		inst := wall.Add(-time.Duration(o) * time.Second)
		if offAt(inst) == o && (!found || inst.Before(best)) {
			best, found = inst, true
		}
	}
	if found {
		return best.UTC()
	}
	before := offAt(wall.Add(-36 * time.Hour))
	return wall.Add(-time.Duration(before) * time.Second).UTC()
}
