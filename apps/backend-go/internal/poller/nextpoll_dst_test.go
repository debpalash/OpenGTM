package poller

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Same Python-derived vectors as autoschedule: the watch poll time must follow
// compute_next_run's wall-clock arithmetic under a non-UTC session TimeZone.
func TestNextPollForInMatchesPythonAcrossDST(t *testing.T) {
	raw, err := os.ReadFile("../autoschedule/testdata/next_run_dst_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Zone string `json:"zone"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation(f.Zone)
	if err != nil {
		t.Fatal(err)
	}
	var vs struct {
		Vectors []struct {
			Interval string `json:"interval"`
			Anchor   string `json:"anchor"`
			Now      string `json:"now"`
			NextUTC  string `json:"next_utc"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	for _, c := range vs.Vectors {
		a, _ := time.Parse(time.RFC3339, c.Anchor)
		now, _ := time.Parse(time.RFC3339, c.Now)
		want, _ := time.Parse(time.RFC3339, c.NextUTC)
		got := NextPollForIn(Watch{Anchor: &a, Interval: c.Interval}, now, Config{DefaultInterval: "daily"}, loc)
		if !got.Equal(want) {
			t.Errorf("%s anchor=%s now=%s: got %s want %s", c.Interval, c.Anchor, c.Now, got.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339))
		}
	}
}

func TestNextPollForNilAnchorWaitsOneInterval(t *testing.T) {
	now := time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)
	got := NextPollFor(Watch{Interval: "hourly"}, now, Config{DefaultInterval: "daily"})
	if want := now.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}
