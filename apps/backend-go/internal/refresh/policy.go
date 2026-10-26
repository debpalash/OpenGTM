// Package refresh ports services/workbook/refresh.py: living-workbook refresh
// policy, stale-cell selection and the refresh_workbook job handler.
package refresh

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// DefaultStalenessDays is the TTL for a field with no staleness_ttl_days entry.
const DefaultStalenessDays = 30

// maxIntervalMinutes bounds a recurring interval to ten years. Python would
// raise OverflowError (or hot-loop on a negative interval) in the enqueue
// path; Go refuses to chain instead (documented deviation).
const maxIntervalMinutes = 10 * 365 * 24 * 60

var intervalMinutes = map[string]int{"hourly": 60, "daily": 1440, "weekly": 10080}

// Clock is the time source for policy and handler scheduling; tests override it.
var Clock = time.Now

// IntervalMinutes mirrors _interval_minutes: hourly/daily/weekly (case
// insensitive), or a number truncated toward zero. ok is false for "no
// interval": a missing/falsy value, an unknown string, a non-scalar, NaN or
// infinity (Python raises), and a value beyond int64 (documented deviation).
func IntervalMinutes(policy map[string]any) (int, bool) {
	v := policy["interval"]
	if !pyTruthy(v) {
		return 0, false
	}
	switch x := v.(type) {
	case bool:
		return 1, true // True is an int in Python
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float64:
		return truncFloat(x)
	case json.Number:
		if i, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return int(i), true
		}
		f, err := x.Float64()
		if err != nil {
			return 0, false
		}
		return truncFloat(f)
	case string:
		m, ok := intervalMinutes[strings.ToLower(x)]
		return m, ok
	}
	return 0, false
}

func truncFloat(f float64) (int, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	t := math.Trunc(f)
	if t >= math.MaxInt64 || t <= math.MinInt64 {
		return 0, false
	}
	return int(t), true
}

// pyTruthy mirrors Python truthiness for decoded JSON values.
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int32:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// schedulable reports whether a recurring chain may be enqueued for minutes.
func schedulable(minutes int, ok bool) bool {
	return ok && minutes > 0 && minutes <= maxIntervalMinutes
}
