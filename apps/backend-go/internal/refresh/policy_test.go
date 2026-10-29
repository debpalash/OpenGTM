package refresh

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
)

func loadVectors(t testing.TB) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]json.RawMessage
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func decodeNum(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

func TestIntervalMinutesVectors(t *testing.T) {
	var cases []struct {
		Policy  map[string]any `json:"policy"`
		Minutes struct {
			Value json.Number `json:"value"`
			Error string      `json:"error"`
		} `json:"minutes"`
		RawMinutes map[string]any `json:"-"`
	}
	if err := decodeNum(loadVectors(t)["interval_minutes"], &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatalf("expected the Python vector set, got %d", len(cases))
	}
	for i, c := range cases {
		got, ok := IntervalMinutes(c.Policy)
		switch {
		case c.Minutes.Error != "":
			// Python raises on NaN/inf; Go reports "no interval" instead.
			if ok {
				t.Errorf("case %d %v: python raised %s, got %d", i, c.Policy, c.Minutes.Error, got)
			}
		case c.Minutes.Value == "":
			if ok {
				t.Errorf("case %d %v: want none, got %d", i, c.Policy, got)
			}
		default:
			want, err := strconv.ParseInt(c.Minutes.Value.String(), 10, 64)
			if err != nil || want > math.MaxInt32*1000 {
				// Documented deviation: minutes beyond int64 are "no interval".
				if ok {
					t.Errorf("case %d %v: overflow must report none, got %d", i, c.Policy, got)
				}
				continue
			}
			if !ok || int64(got) != want {
				t.Errorf("case %d %v: want %d, got %d,%v", i, c.Policy, want, got, ok)
			}
		}
	}
}

func TestIntervalMinutesAcceptsDecodedNumberKinds(t *testing.T) {
	for _, v := range []any{45, int64(45), float64(45), json.Number("45")} {
		if m, ok := IntervalMinutes(map[string]any{"interval": v}); !ok || m != 45 {
			t.Errorf("%T: %d %v", v, m, ok)
		}
	}
}
