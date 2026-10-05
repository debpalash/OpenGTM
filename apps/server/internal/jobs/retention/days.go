package retention

import (
	"fmt"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
)

// Categories lists the retention categories in the order Python's
// DEFAULT_DAYS (and therefore every policy_snapshot) enumerates them.
var Categories = []string{"audit", "llm_usage", "signals", "activation", "audience_history", "agent_results", "outreach_history"}

// DefaultDays and MinDays mirror DEFAULT_DAYS and MIN_DAYS in
// apps/api/services/governance/retention.py.
var (
	DefaultDays = map[string]int64{"audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}
	MinDays     = map[string]int64{"audit": 90, "llm_usage": 30, "signals": 30, "activation": 30, "audience_history": 30, "agent_results": 30, "outreach_history": 30}
)

const maxDays = 3650

// snapshotJSON renders days exactly as json.dumps(dict) would for the
// Python run: Categories order, ", " and ": " separators.
func snapshotJSON(days map[string]int64) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, c := range Categories {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q: %d", c, days[c])
	}
	b.WriteByte('}')
	return b.String()
}

// countsJSON renders deleted_counts like json.dumps of Python's dict, which
// holds only the categories visited so far, in visiting order.
func countsJSON(order []string, counts map[string]int64) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, c := range order {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q: %d", c, counts[c])
	}
	b.WriteByte('}')
	return b.String()
}

// normalizedDays ports normalized_days for the stored JSON text of a policy's
// retention_days (or any decoded value): unknown categories are rejected
// first, then each given value is coerced with int() and range-checked in
// key order. Defaults fill the categories that are absent.
//
// Divergence: a non-empty JSON value that is not an object makes Python fail
// with an AttributeError/TypeError whose text depends on the type; here it is
// one fixed message. The API only ever stores objects.
func normalizedDays(raw []byte) (map[string]int64, error) {
	value, err := jobkit.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("decode retention_days: %w", err)
	}
	return normalizedDaysValue(value)
}

func normalizedDaysValue(value any) (map[string]int64, error) {
	result := make(map[string]int64, len(Categories))
	for k, v := range DefaultDays {
		result[k] = v
	}
	if !jobkit.Truthy(value) {
		return result, nil
	}
	obj, ok := value.(*jobkit.Object)
	if !ok {
		return nil, fmt.Errorf("retention_days must be a JSON object, got %s", jobkit.TypeName(value))
	}
	var unknown []string
	for _, k := range obj.Keys {
		if _, ok := DefaultDays[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, jobkit.Errorf("unsupported retention categories: %s", strings.Join(unknown, ", "))
	}
	for _, k := range obj.Keys {
		days, err := jobkit.Int(obj.Vals[k])
		if err != nil {
			return nil, err
		}
		if days < MinDays[k] || days > maxDays {
			return nil, jobkit.Errorf("%s retention must be %d..%d days", k, MinDays[k], maxDays)
		}
		result[k] = days
	}
	return result, nil
}
