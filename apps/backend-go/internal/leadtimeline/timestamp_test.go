package leadtimeline

import "testing"

// Vectors recorded from Python 3.13 _timeline_timestamp (value is a str).
func TestTimestampParsing(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"2026-08-23T02:30:00Z", 1787452200.0},
		{"2026-08-23T02:30:00+05:30", 1787432400.0},
		{"2026-08-23 02:30:00", 1787452200.0},
		{"2026-08-23T02:30:00.123456", 1787452200.123456},
		{"2026-08-23T02:30:00.5", 1787452200.5},
		{"2026-08-23T02:30:00.1234567", 1787452200.123456},
		{"2026-08-23", 1787443200.0},
		{"20260823", 1787443200.0},
		{"20260823T023000", 1787452200.0},
		{"2026-08-23T02:30", 1787452200.0},
		{"2026-08-23T02", 1787450400.0},
		{"2026-08-23T02:30:00-0800", 1787481000.0},
		{"2026-08-23T02:30:00+05", 1787434200.0},
		{"2026-08-23T02:30:00z", 0},
		{"garbage", 0},
		{"", 0},
		{"1787283000", 0},
		{"2026-13-01", 0},
		{"2026-08-23T24:00:00", 0},
		{"2026-08-23T02:30:60", 0},
		{" 2026-08-23", 0},
		{"2026-08-23T02:30:00+05:30:15", 1787432385.0},
		{"2026-08-23x02:30:00", 1787452200.0},
		{"2026-08-23T0230", 1787452200.0},
		{"2026-08-23T02:30:00.", 0},
		{"2026-08-23T02:30:00+24:00", 0},
		{"0001-01-01T00:00:00Z", -62135596800.0},
		{"9999-12-31T23:59:59.999999Z", 253402300800.0},
		{"2026-08-23T02:30:00,5", 1787452200.5},
	}
	for _, c := range cases {
		if got := ParseTimestamp(c.in); got != c.want {
			t.Errorf("ParseTimestamp(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPyTitle(t *testing.T) {
	cases := map[string]string{
		"entered": "Entered", "exited": "Exited", "": "", "o'neil x": "O'Neil X",
		"ABC def": "Abc Def", "a1b": "A1B", "éco ltd": "Éco Ltd",
	}
	for in, want := range cases {
		if got := pyTitle(in); got != want {
			t.Errorf("pyTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBefore(t *testing.T) {
	nan := func() float64 { z := 0.0; return z / z }()
	if !lessThan(1, "a", 2, "") || lessThan(2, "", 2, "") || !lessThan(2, "a", 2, "b") || lessThan(1, "a", nan, "") {
		t.Fatal("tuple comparison wrong")
	}
}
