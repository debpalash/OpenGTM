package sourcehealth_test

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcehealth"
)

type vectorFile struct {
	Alpha       float64 `json:"alpha"`
	Sequences   []vectorSeq
	OutageGuard []struct {
		Yields      []int   `json:"yields"`
		OutageRatio float64 `json:"outage_ratio"`
		Outage      bool    `json:"outage"`
	} `json:"outage_guard"`
}

type vectorSeq struct {
	Name       string         `json:"name"`
	Initial    map[string]any `json:"initial"`
	Thresholds []int          `json:"thresholds"`
	Steps      []struct {
		Yield  int            `json:"yield"`
		Outage bool           `json:"outage"`
		Now    string         `json:"now"`
		After  map[string]any `json:"after"`
	} `json:"steps"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/transition_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func parseNaive(t *testing.T, s string) time.Time {
	t.Helper()
	for _, l := range []string{"2006-01-02T15:04:05.999999", "2006-01-02T15:04:05"} {
		if ts, err := time.Parse(l, s); err == nil {
			return ts
		}
	}
	t.Fatalf("bad time %q", s)
	return time.Time{}
}

func rowFromInitial(t *testing.T, m map[string]any) *sourcehealth.Row {
	t.Helper()
	r := &sourcehealth.Row{Name: "src", State: "healthy"}
	for k, v := range m {
		switch k {
		case "state":
			r.State = v.(string)
		case "consecutive_zero":
			if v != nil {
				r.ConsecutiveZero = int(v.(float64))
			}
		case "consecutive_nonzero":
			if v != nil {
				r.ConsecutiveNonzero = int(v.(float64))
			}
		case "probes":
			if v != nil {
				r.Probes = int(v.(float64))
			}
		case "ewma_yield":
			if v != nil {
				f := v.(float64)
				r.EwmaYield = &f
			}
		case "manual_override":
			r.ManualOverride = v.(bool)
		case "disabled_at":
			ts := parseNaive(t, v.(string))
			r.DisabledAt = &ts
		case "disabled_reason":
			s := v.(string)
			r.DisabledReason = &s
		default:
			t.Fatalf("unhandled initial key %q", k)
		}
	}
	return r
}

func TestTransitionVectors(t *testing.T) {
	v := loadVectors(t)
	if v.Alpha != sourcehealth.EWMAAlpha {
		t.Fatalf("alpha %v != %v", v.Alpha, sourcehealth.EWMAAlpha)
	}
	if len(v.Sequences) != 60 {
		t.Fatalf("expected 60 sequences, got %d", len(v.Sequences))
	}
	for _, seq := range v.Sequences {
		t.Run(seq.Name, func(t *testing.T) {
			th := sourcehealth.DefaultThresholds
			if seq.Thresholds != nil {
				th = sourcehealth.Thresholds{Degrade: seq.Thresholds[0], Disable: seq.Thresholds[1], Recover: seq.Thresholds[2]}
			}
			r := rowFromInitial(t, seq.Initial)
			for i, st := range seq.Steps {
				sourcehealth.ApplyTransition(r, st.Yield, st.Outage, parseNaive(t, st.Now), th)
				b, err := json.Marshal(r.ToAPI())
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				_ = json.Unmarshal(b, &got)
				delete(got, "updated_at")
				want := map[string]any{}
				for k, x := range st.After {
					want[k] = x
				}
				raw := want["ewma_yield_raw"].(float64)
				delete(want, "ewma_yield_raw")
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("step %d:\n got %v\nwant %v", i, got, want)
				}
				if r.EwmaYield == nil || math.Float64bits(*r.EwmaYield) != math.Float64bits(raw) {
					t.Fatalf("step %d raw ewma %v want %v", i, r.EwmaYield, raw)
				}
			}
		})
	}
}

func TestOutageGuardVectors(t *testing.T) {
	for i, c := range loadVectors(t).OutageGuard {
		if got := sourcehealth.OutageGuard(c.Yields, c.OutageRatio); got != c.Outage {
			t.Errorf("case %d %v ratio %v: got %v want %v", i, c.Yields, c.OutageRatio, got, c.Outage)
		}
	}
}

var t0 = time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC)

func TestOutageAppliesNoZeroTransitions(t *testing.T) {
	r := &sourcehealth.Row{Name: "s", State: "healthy", ConsecutiveZero: 5}
	for i := 0; i < 10; i++ {
		sourcehealth.ApplyTransition(r, 0, true, t0, sourcehealth.DefaultThresholds)
	}
	if r.State != "healthy" || r.ConsecutiveZero != 5 || r.Probes != 10 || r.LastOutageAt == nil {
		t.Fatalf("outage zero must not transition: %+v", r)
	}
}

func TestNonzeroDuringOutageStillRecovers(t *testing.T) {
	r := &sourcehealth.Row{Name: "s", State: "auto_disabled", ConsecutiveZero: 6}
	for i := 0; i < 2; i++ {
		sourcehealth.ApplyTransition(r, 3, true, t0, sourcehealth.DefaultThresholds)
	}
	if r.State != "healthy" || r.DisabledAt != nil || r.DisabledReason != nil {
		t.Fatalf("expected recovery during outage: %+v", r)
	}
}

func TestManualOverrideNeverAutoDisabled(t *testing.T) {
	r := &sourcehealth.Row{Name: "s", State: "healthy", ManualOverride: true}
	for i := 0; i < 20; i++ {
		sourcehealth.ApplyTransition(r, 0, false, t0, sourcehealth.DefaultThresholds)
	}
	if r.State != "degraded" || r.DisabledAt != nil || r.ConsecutiveZero != 20 {
		t.Fatalf("manual override must cap at degraded: %+v", r)
	}
}

func TestEWMARoundingBankers(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0.0625, 0.062}, {0.1875, 0.188}, {0.0005, 0.001}, {2.5, 2.5}, {1.2345, 1.234}, {0.1234999, 0.123}, {-1.5999999999999999, -1.6},
	}
	for _, c := range cases {
		r := &sourcehealth.Row{Name: "s", State: "healthy", EwmaYield: &c.in}
		got, _ := r.ToAPI().Get("ewma_yield")
		if got.(float64) != c.want {
			t.Errorf("round(%v,3) = %v want %v", c.in, got, c.want)
		}
	}
}

func TestToAPIKeyOrderAndTimestamps(t *testing.T) {
	us := time.Date(2026, 8, 30, 3, 0, 0, 123456000, time.UTC)
	r := &sourcehealth.Row{Name: "s", State: "healthy", LastProbeAt: &us, LastOkAt: &t0}
	o := r.ToAPI()
	want := []string{"name", "state", "consecutive_zero", "consecutive_nonzero", "last_yield", "ewma_yield", "probes",
		"last_probe_at", "last_ok_at", "last_outage_at", "disabled_at", "disabled_reason", "manual_override", "updated_at"}
	if !reflect.DeepEqual(o.Keys(), want) {
		t.Fatalf("keys %v", o.Keys())
	}
	b, _ := json.Marshal(o)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["last_probe_at"] != "2026-08-30T03:00:00.123456" || m["last_ok_at"] != "2026-08-29T03:00:00" || m["updated_at"] != nil {
		t.Fatalf("timestamps: %s", b)
	}
}

func TestThresholdsFromEnv(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := sourcehealth.ThresholdsFromEnv(get(nil)); got != (sourcehealth.Thresholds{Degrade: 3, Disable: 6, Recover: 2}) {
		t.Fatalf("defaults: %+v", got)
	}
	got := sourcehealth.ThresholdsFromEnv(get(map[string]string{
		"SOURCE_HEALTH_DEGRADE_THRESHOLD": "1", "SOURCE_HEALTH_DISABLE_THRESHOLD": " 4 ", "SOURCE_HEALTH_RECOVER_THRESHOLD": "x"}))
	if got != (sourcehealth.Thresholds{Degrade: 1, Disable: 4, Recover: 2}) {
		t.Fatalf("env: %+v", got)
	}
}
