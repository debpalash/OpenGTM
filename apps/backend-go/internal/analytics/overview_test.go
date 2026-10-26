package analytics

import (
	"encoding/json"
	"testing"
)

// Expected values come from CPython: round(n / total * 100, 1).
func TestPctBankersRounding(t *testing.T) {
	for _, c := range []struct {
		n, total int64
		want     string
	}{
		{1, 8, "12.5"}, {1, 24, "4.2"}, {1, 16, "6.2"}, {3, 16, "18.8"}, {1, 32, "3.1"}, {5, 16, "31.2"},
		{1, 3, "33.3"}, {2, 3, "66.7"}, {7, 7, "100.0"}, {0, 5, "0.0"}, {1, 800, "0.1"}, {1, 7, "14.3"},
		{0, 0, "0"}, {5, 0, "0"}, // total == 0 -> int 0, not 0.0
	} {
		b, err := json.Marshal(Pct(c.n, c.total))
		if err != nil || string(b) != c.want {
			t.Errorf("Pct(%d,%d) = %s (%v), want %s", c.n, c.total, b, err, c.want)
		}
	}
}

func TestEmptyShapes(t *testing.T) {
	for _, c := range []struct {
		name string
		v    any
		want string
	}{
		{"collection", Collection(), `{"jobs_by_day":[],"leads_by_day":[],"avg_leads_per_job":0}`},
		{"llm", LLM(), `{"by_day":[],"by_provider":[],"total_tokens":0,"total_calls":0}`},
	} {
		b, _ := json.Marshal(c.v)
		if string(b) != c.want {
			t.Errorf("%s: %s", c.name, b)
		}
	}
}
