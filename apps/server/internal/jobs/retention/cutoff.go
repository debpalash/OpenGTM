package retention

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

const microsPerDay = int64(86400) * 1_000_000

// cutoffFor ports the cutoff arithmetic of _targets for one category:
// `now - timedelta(days=days[category])`, where days is the run's
// policy_snapshot and now is the naive wall-clock started_at (UTC-labelled).
// Failures carry Python's exception text (KeyError, TypeError, OverflowError),
// because the handler records str(exc) as the run error.
func cutoffFor(snapshot any, category string, now time.Time) (time.Time, error) {
	var raw any
	switch s := snapshot.(type) {
	case *object:
		v, ok := s.vals[category]
		if !ok {
			return time.Time{}, pyErrorf("%s", pyRepr(category)) // KeyError('audit')
		}
		raw = v
	case []any:
		return time.Time{}, pyErrorf("list indices must be integers or slices, not str")
	case string:
		return time.Time{}, pyErrorf("string indices must be integers, not 'str'")
	default:
		return time.Time{}, pyErrorf("'%s' object is not subscriptable", pyTypeName(snapshot))
	}
	days, rem, err := timedeltaDays(raw)
	if err != nil {
		return time.Time{}, err
	}
	// Whole days are subtracted calendar-wise (UTC, so exactly 24h each);
	// time.Duration could not hold Python's full +-999999999 day range.
	cutoff := now.AddDate(0, 0, -int(days)).Add(-time.Duration(rem) * time.Microsecond)
	if y := cutoff.Year(); y < 1 || y > 9999 {
		return time.Time{}, pyErrorf("date value out of range")
	}
	return cutoff, nil
}

// timedeltaDays ports timedelta(days=value) as whole days plus a signed
// microsecond remainder.
func timedeltaDays(v any) (days, remMicros int64, err error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, 0, nil
		}
		return 0, 0, nil
	case json.Number:
		if !isFloatLiteral(t) {
			n, perr := strconv.ParseInt(string(t), 10, 64)
			if perr != nil || n > 999999999 || n < -999999999 {
				return 0, 0, pyErrorf("days=%s; must have magnitude <= 999999999", string(t))
			}
			return n, 0, nil
		}
		f, _ := strconv.ParseFloat(string(t), 64)
		if math.IsInf(f, 0) {
			return 0, 0, pyErrorf("cannot convert float infinity to integer")
		}
		if math.Abs(f) > 999999999 {
			return 0, 0, pyErrorf("days=%d; must have magnitude <= 999999999", clampTrunc(f))
		}
		whole := math.Trunc(f)
		return int64(whole), int64(math.RoundToEven((f - whole) * float64(microsPerDay))), nil
	}
	return 0, 0, pyErrorf("unsupported type for timedelta days component: %s", pyTypeName(v))
}

// epochSeconds ports naive_datetime.timestamp(): the wall clock is read in
// the process's local zone, exactly as Python does for the "epoch" storage.
func epochSeconds(wall time.Time) float64 {
	local := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, time.Local)
	return float64(local.Unix()) + float64(wall.Nanosecond()/1000)/1e6
}
