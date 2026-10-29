package api

import (
	"net/url"
	"testing"
)

func TestParseAuditQueryErrorOrderAndLastWins(t *testing.T) {
	q, errs := parseAuditQuery(url.Values{"actor_user_id": {"x"}, "since": {"garbage"}, "before": {"2030-13-45"}, "limit": {"0"}}, true)
	_ = q
	var locs []string
	for _, e := range errs {
		locs = append(locs, e.Loc[1].(string))
	}
	want := []string{"actor_user_id", "since", "before", "limit"}
	if len(locs) != len(want) {
		t.Fatalf("locs %v", locs)
	}
	for i := range want {
		if locs[i] != want[i] {
			t.Fatalf("locs %v", locs)
		}
	}
	q, errs = parseAuditQuery(url.Values{"outcome": {"a", "denied"}, "limit": {"5", "7"}}, true)
	if len(errs) != 0 || q.filter.Outcome != "denied" || q.limit != 7 {
		t.Fatalf("%+v %v", q, errs)
	}
	if _, errs = parseAuditQuery(url.Values{"limit": {"0"}}, false); len(errs) != 0 {
		t.Fatal("export must ignore limit")
	}
}
