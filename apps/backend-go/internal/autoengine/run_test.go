package autoengine

import "testing"

func TestRunStatusDerivationVectors(t *testing.T) {
	cases := []struct {
		name string
		dry  bool
		st   stats
		want string
	}{
		{"dry run wins", true, stats{attempted: 3, failed: 3}, "dry_run"},
		{"failed and succeeded", false, stats{attempted: 3, succeeded: 2, failed: 1}, "partial"},
		{"all failed", false, stats{attempted: 3, failed: 3}, "failed"},
		{"failed without an attempt (crash before attempt)", false, stats{failed: 1}, "completed"},
		{"skipped only after attempts", false, stats{attempted: 3, skipped: 3}, "partial"},
		{"skipped with success", false, stats{attempted: 3, succeeded: 2, skipped: 1}, "completed"},
		{"replays only (no attempts)", false, stats{skipped: 3}, "completed"},
		{"skipped, failed and success", false, stats{attempted: 3, succeeded: 1, skipped: 1, failed: 1}, "partial"},
		{"nothing", false, stats{}, "completed"},
		{"all success", false, stats{attempted: 2, succeeded: 2}, "completed"},
	}
	for _, c := range cases {
		if got := deriveStatus(c.dry, c.st); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestRound4MatchesPython(t *testing.T) {
	// values from Python: round(0.1+0.2, 4) == 0.3, round(0.05+0.05, 4) == 0.1,
	// round(2.675, 2)-style binary ties follow the binary value
	for _, c := range []struct{ in, want float64 }{{0.1 + 0.2, 0.3}, {0.05 + 0.05, 0.1}, {0.12344, 0.1234}, {0.12346, 0.1235}, {0, 0}, {1.00004, 1}} {
		if got := round4(c.in); got != c.want {
			t.Errorf("round4(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestActivityMessageMatchesPython(t *testing.T) {
	got := activityMessage("rule", "partial", 3, stats{succeeded: 2, skipped: 1, failed: 0}, 0.1+0.2)
	want := "automation 'rule' partial: 3 matched, 2 ok, 1 skipped, 0 failed, $0.3"
	if got != want {
		t.Fatalf("%q", got)
	}
	if got := activityMessage("é's", "completed", 0, stats{}, 0); got != "automation 'é's' completed: 0 matched, 0 ok, 0 skipped, 0 failed, $0.0" {
		t.Fatalf("%q", got)
	}
}

func TestIntRowID(t *testing.T) {
	for _, c := range []struct {
		in   any
		want int32
		ok   bool
	}{{"12", 12, true}, {" 7 ", 7, true}, {"abc", 0, false}, {"", 0, false}, {"0", 0, false}, {"99999999999", 0, false}, {float64(3), 3, true}, {nil, 0, false}, {true, 0, false}} {
		got, ok := intRowID(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("intRowID(%v) = %d,%v", c.in, got, ok)
		}
	}
}

func TestPayloadTargetsCapAndShape(t *testing.T) {
	raw := []any{map[string]any{"workbook_id": "a", "row_id": "1"}, "junk", map[string]any{"workbook_id": "a", "row_id": "x"}, map[string]any{"workbook_id": "b", "row_id": "2"}}
	refs, ok := payloadTargets(raw, 500)
	if !ok || len(refs) != 2 || refs[1].workbookID != "b" {
		t.Fatalf("%+v %v", refs, ok)
	}
	refs, _ = payloadTargets(raw, 1)
	if len(refs) != 1 {
		t.Fatalf("cap: %+v", refs)
	}
	if _, ok := payloadTargets([]any{}, 5); ok {
		t.Fatal("empty list is a sweep")
	}
	if _, ok := payloadTargets("x", 5); ok {
		t.Fatal("non-list is a sweep")
	}
}
