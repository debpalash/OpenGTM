package autoschedule

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

type dstFile struct {
	Zone    string `json:"zone"`
	Vectors []struct {
		Interval string `json:"interval"`
		Anchor   string `json:"anchor"`
		Now      string `json:"now"`
		NextUTC  string `json:"next_utc"`
		Next     string `json:"next"`
	} `json:"vectors"`
}

// Expected values come from legacy/python compute_next_run with ZoneInfo
// anchors (what psycopg returns under a non-UTC session TimeZone): wall-clock
// arithmetic in the anchor's zone, absolute elapsed time.
func TestNextRunInMatchesPythonWallClockAcrossDST(t *testing.T) {
	raw, err := os.ReadFile("testdata/next_run_dst_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f dstFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation(f.Zone)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range f.Vectors {
		got := NextRunIn(parseISO(t, c.Anchor), c.Interval, parseISO(t, c.Now), loc)
		if want := parseISO(t, c.NextUTC); !got.Equal(want) {
			t.Errorf("%s anchor=%s now=%s: got %s want %s (python %s)", c.Interval, c.Anchor, c.Now,
				got.UTC().Format(time.RFC3339), want.Format(time.RFC3339), c.Next)
		}
	}
}

// A NULL anchor means anchor=now; Python reads the clock again inside
// compute_next_run, so the next run is one interval ahead, never "due now".
func TestTriggerNextNilAnchorWaitsOneInterval(t *testing.T) {
	now := time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)
	for iv, d := range map[string]time.Duration{"hourly": time.Hour, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour} {
		got := Trigger{Interval: iv}.next(now, time.UTC)
		if want := now.Add(d); !got.Equal(want) {
			t.Errorf("%s: got %s want %s", iv, got, want)
		}
	}
}
