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

// Vectors (audit F1): absolute-instant arithmetic, anchors written in New York
// offsets around the 2026 transitions; the zone argument must not matter.
func TestNextRunInAbsoluteInstantsAcrossDST(t *testing.T) {
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
		got := Trigger{Interval: iv}.next(now)
		if want := now.Add(d); !got.Equal(want) {
			t.Errorf("%s: got %s want %s", iv, got, want)
		}
	}
}

// Daily intervals across both 2026 New York transitions stay exactly 24h apart
// whatever loc is passed, and fire keys render in UTC.
func TestNextRunInEqualIntervalsAcrossDST(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	for _, c := range []struct{ anchor, now string }{
		{"2026-03-07T07:00:00-05:00", "2026-03-07T13:00:00+00:00"},
		{"2026-10-31T07:00:00-04:00", "2026-10-31T13:00:00+00:00"},
	} {
		a := parseISO(t, c.anchor)
		now := parseISO(t, c.now)
		n1 := NextRunIn(a, "daily", now, loc)
		n2 := NextRunIn(a, "daily", n1, loc)
		n3 := NextRunIn(a, "daily", n2, loc)
		if n2.Sub(n1) != 24*time.Hour || n3.Sub(n2) != 24*time.Hour {
			t.Errorf("anchor %s: intervals %s, %s want 24h", c.anchor, n2.Sub(n1), n3.Sub(n2))
		}
		if got, want := FireKeyIn("t1", n1, loc), FireKey("t1", n1); got != want {
			t.Errorf("fire key %q want UTC %q", got, want)
		}
	}
}
