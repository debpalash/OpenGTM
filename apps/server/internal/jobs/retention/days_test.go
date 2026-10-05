package retention

import (
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
)

func TestNormalizedDays(t *testing.T) {
	ok := []struct {
		in   string
		want string
	}{
		{`{}`, `{"audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}`},
		{`null`, `{"audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}`},
		{`[]`, `{"audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}`},
		{`{"outreach_history": 3650, "audit": 90}`, `{"audit": 90, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 3650}`},
		{`{"audit": "100", "signals": 99.9, "activation": true, "llm_usage": " +4_0 "}`, ``}, // activation=True -> 1 < 30
	}
	if got, err := normalizedDays([]byte(ok[0].in)); err != nil || snapshotJSON(got) != ok[0].want {
		t.Fatalf("defaults: %v %v", got, err)
	}
	for _, c := range ok[1:4] {
		got, err := normalizedDays([]byte(c.in))
		if err != nil || snapshotJSON(got) != c.want {
			t.Errorf("%s -> %s, %v; want %s", c.in, snapshotJSON(got), err, c.want)
		}
	}

	bad := []struct{ in, msg string }{
		{`{"leads": 1}`, "unsupported retention categories: leads"},
		{`{"zeta": 1, "alpha": 2, "audit": 100}`, "unsupported retention categories: alpha, zeta"},
		{`{"audit": 89}`, "audit retention must be 90..3650 days"},
		{`{"audit": 3651}`, "audit retention must be 90..3650 days"},
		{`{"signals": 29.99}`, "signals retention must be 30..3650 days"},
		{`{"signals": 99999999999999999999}`, "signals retention must be 30..3650 days"},
		{`{"signals": true}`, "signals retention must be 30..3650 days"},
		{`{"signals": "abc"}`, "invalid literal for int() with base 10: 'abc'"},
		{`{"signals": "1__0"}`, "invalid literal for int() with base 10: '1__0'"},
		{`{"signals": "it's"}`, `invalid literal for int() with base 10: "it's"`},
		{`{"signals": "1.5"}`, "invalid literal for int() with base 10: '1.5'"},
		{`{"signals": ""}`, "invalid literal for int() with base 10: ''"},
		{`{"signals": null}`, "int() argument must be a string, a bytes-like object or a real number, not 'NoneType'"},
		{`{"signals": [1]}`, "int() argument must be a string, a bytes-like object or a real number, not 'list'"},
		{`{"signals": {"a": 1}}`, "int() argument must be a string, a bytes-like object or a real number, not 'dict'"},
		// first invalid value in stored key order wins
		{`{"signals": 1, "audit": 1}`, "signals retention must be 30..3650 days"},
		{`{"audit": 1, "signals": 1}`, "audit retention must be 90..3650 days"},
	}
	for _, c := range bad {
		_, err := normalizedDays([]byte(c.in))
		if err == nil || err.Error() != c.msg {
			t.Errorf("%s -> %v; want %q", c.in, err, c.msg)
		}
	}

	good, err := normalizedDays([]byte(`{"audit": "100", "signals": 99.9, "llm_usage": " +4_0 "}`))
	if err != nil || good["audit"] != 100 || good["signals"] != 99 || good["llm_usage"] != 40 {
		t.Errorf("coercions: %v %v", good, err)
	}
}

func TestCutoffFor(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 123456000, time.UTC)
	snap := func(s string) any {
		v, err := jobkit.Decode([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	got, err := cutoffFor(snap(`{"audit": 365}`), "audit", now)
	if err != nil || !got.Equal(time.Date(2025, 6, 15, 12, 0, 0, 123456000, time.UTC)) {
		t.Errorf("365d: %v %v", got, err)
	}
	if got, err = cutoffFor(snap(`{"audit": 0.5}`), "audit", now); err != nil || !got.Equal(now.Add(-12*time.Hour)) {
		t.Errorf("0.5d: %v %v", got, err)
	}
	if got, err = cutoffFor(snap(`{"audit": true}`), "audit", now); err != nil || !got.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("True: %v %v", got, err)
	}
	if got, err = cutoffFor(snap(`{"audit": -5}`), "audit", now); err != nil || !got.Equal(now.Add(5*24*time.Hour)) {
		t.Errorf("-5d: %v %v", got, err)
	}
	for _, c := range []struct{ snap, cat, msg string }{
		{`{"x": 1}`, "audit", "'audit'"},
		{`{"audit": "7"}`, "audit", "unsupported type for timedelta days component: str"},
		{`{"audit": 1000000000}`, "audit", "days=1000000000; must have magnitude <= 999999999"},
		{`{"audit": 999999999}`, "audit", "date value out of range"},
		{`"x"`, "audit", "string indices must be integers, not 'str'"},
		{`5`, "audit", "'int' object is not subscriptable"},
	} {
		if _, err := cutoffFor(snap(c.snap), c.cat, now); err == nil || err.Error() != c.msg {
			t.Errorf("%s: %v; want %q", c.snap, err, c.msg)
		}
	}
}

func TestEpochCutoffUsesLocalZone(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	prev := time.Local
	defer func() { time.Local = prev }()
	wall := time.Date(2026, 6, 15, 12, 0, 0, 250000000, time.UTC)

	time.Local = time.UTC
	utc := epochSeconds(wall)
	time.Local = ist
	local := epochSeconds(wall)
	// Python: naive.timestamp() interprets the wall clock in the local zone,
	// so IST (+05:30) is 19800 seconds earlier than UTC for the same wall time.
	if utc-local != 19800 {
		t.Errorf("utc=%v local=%v", utc, local)
	}
	if utc != 1781524800.25 {
		t.Errorf("utc epoch = %v", utc)
	}
}

func TestPayloadParsing(t *testing.T) {
	pl, err := parsePayload([]byte(`{"workspace_id": "ws", "run_id": ""}`))
	if err != nil || pl.workspaceID != "ws" || pl.runID != "" {
		t.Errorf("%+v %v", pl, err)
	}
	if _, err := parsePayload([]byte(`[1]`)); err == nil {
		t.Error("array payload accepted")
	}
	if pl, _ := parsePayload([]byte(`{"workspace_id": 0, "run_id": null}`)); pl.workspaceID != "" || pl.runID != "" {
		t.Errorf("falsy values must read as empty: %+v", pl)
	}
	if pl, _ := parsePayload([]byte(`{"workspace_id": 42}`)); pl.workspaceID != "42" {
		t.Errorf("number: %+v", pl)
	}
}

func TestCountsJSONMatchesPythonDumps(t *testing.T) {
	got := countsJSON([]string{"audit", "signals"}, map[string]int64{"audit": 3, "signals": 0})
	if got != `{"audit": 3, "signals": 0}` {
		t.Error(got)
	}
	if got := countsJSON(nil, nil); got != "{}" {
		t.Error(got)
	}
	if !strings.HasPrefix(snapshotJSON(DefaultDays), `{"audit": 365, `) {
		t.Error(snapshotJSON(DefaultDays))
	}
}
