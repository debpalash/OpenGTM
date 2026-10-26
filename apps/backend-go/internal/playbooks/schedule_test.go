package playbooks

import (
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// Expected strings are datetime.isoformat() of an aware UTC datetime (Python).
func TestScheduleFireKeyFormatMatchesPython(t *testing.T) {
	cases := []struct {
		id   string
		at   time.Time
		want string
	}{
		{"pb1", time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC), "playbook_schedule:pb1:2026-06-15T12:30:00+00:00"},
		{"pb1", time.Date(2026, 6, 15, 12, 30, 0, 123456000, time.UTC), "playbook_schedule:pb1:2026-06-15T12:30:00.123456+00:00"},
		{"pb1", time.Date(2026, 6, 15, 12, 30, 0, 1000, time.UTC), "playbook_schedule:pb1:2026-06-15T12:30:00.000001+00:00"},
		// a non-UTC instant renders in UTC (Python's now is UTC).
		{"x", time.Date(2026, 6, 15, 18, 0, 0, 0, time.FixedZone("IST", 5*3600+1800)), "playbook_schedule:x:2026-06-15T12:30:00+00:00"},
	}
	for _, c := range cases {
		if got := ScheduleFireKey(c.id, c.at); got != c.want {
			t.Errorf("ScheduleFireKey(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestScheduleDelayClamp(t *testing.T) {
	for in, want := range map[int]int{1: 15, 14: 15, 15: 15, 60: 60, 10080: 10080, 10081: 10080, 1 << 30: 10080} {
		if got := scheduleDelay(in); got != time.Duration(want)*time.Minute {
			t.Errorf("scheduleDelay(%d) = %v, want %d min", in, got, want)
		}
	}
}

func TestExecutorForPlaybooks(t *testing.T) {
	ex := config.ExecutorSettings{Enabled: true, Workspaces: map[string]bool{"w1": true}}
	on := config.GovernanceSettings{GoExecutorPlaybooks: true}
	cases := []struct {
		ex   config.ExecutorSettings
		g    config.GovernanceSettings
		ws   string
		want string
	}{
		{ex, on, "w1", "go"},
		{ex, on, "w2", "python"},
		{ex, on, "", "python"},
		{ex, config.GovernanceSettings{}, "w1", "python"},
		{config.ExecutorSettings{Workspaces: ex.Workspaces}, on, "w1", "python"},
	}
	for i, c := range cases {
		if got := ExecutorFor(c.ex, c.g, c.ws); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
}
