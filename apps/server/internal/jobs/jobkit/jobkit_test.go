package jobkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIsoFormatMatchesPython(t *testing.T) {
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC), "2026-06-15T12:00:00+00:00"},
		{time.Date(2026, 6, 15, 12, 0, 0, 123456000, time.UTC), "2026-06-15T12:00:00.123456+00:00"},
		{time.Date(2026, 6, 15, 12, 0, 0, 100000, time.UTC), "2026-06-15T12:00:00.000100+00:00"},
		// sub-microsecond precision is dropped like a Python datetime would
		{time.Date(2026, 6, 15, 12, 0, 0, 999, time.UTC), "2026-06-15T12:00:00+00:00"},
		{time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 5*3600+1800)), "2026-01-01T21:34:05+00:00"},
		{time.Date(7, 1, 2, 3, 4, 5, 0, time.UTC), "0007-01-02T03:04:05+00:00"},
	}
	for _, c := range cases {
		if got := IsoFormat(c.in); got != c.want {
			t.Errorf("IsoFormat(%v) = %s, want %s", c.in, got, c.want)
		}
	}
	if got := Micros(time.Date(2026, 6, 15, 12, 0, 0, 1999, time.UTC)); got.Nanosecond() != 1000 {
		t.Errorf("Micros kept %dns", got.Nanosecond())
	}
}

func TestClampMinutes(t *testing.T) {
	p := func(n int64) *int64 { return &n }
	for _, c := range []struct {
		in   *int64
		want int64
	}{
		{nil, 60}, {p(0), 60}, {p(1), 15}, {p(-5), 15}, {p(15), 15}, {p(90), 90},
		{p(10080), 10080}, {p(10081), 10080}, {p(1 << 40), 10080},
	} {
		if got := ClampMinutes(c.in, 60, 15, 10080); got != c.want {
			t.Errorf("ClampMinutes(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPythonValueSemantics(t *testing.T) {
	v, err := Decode([]byte(`{"b": 1, "a": 2.5, "b": 3, "s": " 4_0 ", "n": null, "l": [], "o": {}}`))
	if err != nil {
		t.Fatal(err)
	}
	obj := v.(*Object)
	if strings.Join(obj.Keys, ",") != "b,a,s,n,l,o" {
		t.Errorf("key order = %v", obj.Keys)
	}
	if got, _ := obj.Get("b"); Str(got) != "3" {
		t.Errorf("later duplicate must win, got %v", got)
	}
	for in, want := range map[string]bool{`0`: false, `0.0`: false, `""`: false, `[]`: false, `{}`: false, `null`: false, `false`: false,
		`1`: true, `"0"`: true, `[0]`: true, `{"a":1}`: true, `true`: true} {
		v, err := Decode([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if Truthy(v) != want {
			t.Errorf("Truthy(%s) = %v", in, !want)
		}
	}
	strs := map[string]string{`"ws"`: "ws", `42`: "42", `0`: "", `null`: "", `true`: "True", `false`: "", `[1]`: "", `{"a":1}`: ""}
	for in, want := range strs {
		v, _ := Decode([]byte(in))
		if got := Str(v); got != want {
			t.Errorf("Str(%s) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int64{`" +4_0 "`: 40, `99.9`: 99, `-99.9`: -99, `true`: 1, `"7"`: 7, `12345678901234567890`: 1<<63 - 1} {
		v, _ := Decode([]byte(in))
		if got, err := Int(v); err != nil || got != want {
			t.Errorf("Int(%s) = %d, %v; want %d", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{
		`"abc"`:  "invalid literal for int() with base 10: 'abc'",
		`null`:   "int() argument must be a string, a bytes-like object or a real number, not 'NoneType'",
		`[1]`:    "not 'list'",
		`"1__0"`: "invalid literal for int() with base 10: '1__0'",
	} {
		v, _ := Decode([]byte(in))
		if _, err := Int(v); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("Int(%s) error = %v, want %q", in, err, msg)
		}
	}
	if _, err := Decode([]byte(`{"a":1} x`)); err == nil {
		t.Error("trailing data accepted")
	}
	if Repr("it's") != `"it's"` || Repr("a\nb") != `'a\nb'` || Repr(`'"`) != `'\'"'` {
		t.Errorf("Repr: %s %s %s", Repr("it's"), Repr("a\nb"), Repr(`'"`))
	}
	if got := TruncateChars(strings.Repeat("é", 1200), 1000); len([]rune(got)) != 1000 {
		t.Errorf("TruncateChars kept %d chars", len([]rune(got)))
	}
	var e error = Errorf("x %d", 1)
	var pe *Error
	if !errors.As(e, &pe) || pe.Msg != "x 1" {
		t.Error("Errorf must produce a *Error")
	}
}

func TestKeysetPagesWalksAndToleratesDeletion(t *testing.T) {
	rows := make([]int64, 0, 23)
	for i := int64(1); i <= 23; i++ {
		rows = append(rows, i*2)
	}
	fetch := func(_ context.Context, after int64, limit int) ([]int64, error) {
		var out []int64
		for _, r := range rows {
			if r > after && len(out) < limit {
				out = append(out, r)
			}
		}
		return out, nil
	}
	var seen []int64
	pages := 0
	err := KeysetPages(context.Background(), 5, fetch, func(r int64) int64 { return r }, func(page []int64) error {
		pages++
		seen = append(seen, page...)
		// the callback may delete what it has seen without disturbing the walk
		rows = rows[len(page):]
		return nil
	})
	if err != nil || pages != 5 || len(seen) != 23 || seen[0] != 2 || seen[22] != 46 {
		t.Fatalf("pages=%d seen=%v err=%v", pages, seen, err)
	}

	boom := errors.New("boom")
	rows = []int64{1, 2, 3}
	if err := KeysetPages(context.Background(), 2, fetch, func(r int64) int64 { return r },
		func([]int64) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("each error not returned: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := KeysetPages(ctx, 2, fetch, func(r int64) int64 { return r }, func([]int64) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context not honoured: %v", err)
	}
}
