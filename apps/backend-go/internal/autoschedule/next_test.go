package autoschedule

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

type vectorFile struct {
	Vectors []struct {
		Anchor   string `json:"anchor"`
		Interval string `json:"interval"`
		Now      string `json:"now"`
		NextRun  string `json:"next_run_at"`
	} `json:"vectors"`
	IntervalDelta map[string]float64 `json:"interval_delta_seconds"`
	FireKeys      []struct {
		TriggerID string `json:"trigger_id"`
		NextRunAt string `json:"next_run_at"`
		FireKey   string `json:"fire_key"`
	} `json:"fire_keys"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/next_run_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func parseISO(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

func TestNextRunVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.Vectors) != 60 {
		t.Fatalf("expected 60 vectors, got %d", len(v.Vectors))
	}
	for _, c := range v.Vectors {
		got := NextRun(parseISO(t, c.Anchor), c.Interval, parseISO(t, c.Now))
		if want := parseISO(t, c.NextRun); !got.Equal(want) {
			t.Errorf("anchor=%s interval=%q now=%s: got %s want %s", c.Anchor, c.Interval, c.Now, got.Format(time.RFC3339Nano), c.NextRun)
		}
	}
	for k, secs := range v.IntervalDelta {
		if got := IntervalDelta(k); got != time.Duration(secs*float64(time.Second)) {
			t.Errorf("IntervalDelta(%q)=%v want %vs", k, got, secs)
		}
	}
}

func TestFireKeyTextEqualsPython(t *testing.T) {
	v := loadVectors(t)
	if len(v.FireKeys) == 0 {
		t.Fatal("no fire key vectors")
	}
	for _, c := range v.FireKeys {
		if got := FireKey(c.TriggerID, parseISO(t, c.NextRunAt)); got != c.FireKey {
			t.Errorf("FireKey: got %q want %q", got, c.FireKey)
		}
	}
	// Python isoformat appends microseconds only when non-zero.
	ts := time.Date(2026, 8, 29, 8, 0, 0, 1000, time.UTC)
	if got, want := FireKey("t", ts), "sched:t:2026-08-29T08:00:00.000001+00:00"; got != want {
		t.Errorf("micro: got %q want %q", got, want)
	}
	// A non-UTC instant is rendered in UTC.
	ist := time.FixedZone("IST", 5*3600+1800)
	if got, want := FireKey("t", time.Date(2026, 8, 29, 13, 30, 0, 0, ist)), "sched:t:2026-08-29T08:00:00+00:00"; got != want {
		t.Errorf("tz: got %q want %q", got, want)
	}
}
