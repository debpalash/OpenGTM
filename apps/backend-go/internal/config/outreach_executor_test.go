package config

import "testing"

func TestOutreachExecutorGates(t *testing.T) {
	base := map[string]string{
		"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "ws-1, ws-2", "GO_EXECUTOR_OUTREACH_SEND": "1", "GO_EXECUTOR_OUTREACH_INBOUND": "1", "APP_ENV": "dev",
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	cases := []struct {
		name    string
		env     map[string]string
		job, ws string
		want    string
	}{
		{"all on send", base, "send", "ws-1", "go"},
		{"all on inbound", base, "outreach_inbound_poll", "ws-2", "go"},
		{"master off", with("GO_EXECUTOR_ENABLED", "false"), "send", "ws-1", "python"},
		{"not in cohort", base, "send", "ws-9", "python"},
		{"empty workspace", base, "send", "", "python"},
		{"send flag off", with("GO_EXECUTOR_OUTREACH_SEND", "0"), "send", "ws-1", "python"},
		{"inbound flag off keeps send", with("GO_EXECUTOR_OUTREACH_INBOUND", "0"), "send", "ws-1", "go"},
		{"inbound flag off", with("GO_EXECUTOR_OUTREACH_INBOUND", "0"), "outreach_inbound_poll", "ws-1", "python"},
		{"empty cohort", with("GO_EXECUTOR_WORKSPACES", ""), "send", "ws-1", "python"},
	}
	for _, c := range cases {
		get := func(k string) string { return c.env[k] }
		ex := ExecutorCohortFrom(get)
		o, err := loadOutreach(get)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ex.OutreachExecutor(o, c.job, c.ws)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q err=%v want %q", c.name, got, err, c.want)
		}
	}
	if _, err := (ExecutorSettings{}).OutreachExecutor(OutreachSettings{}, "refresh_workbook", "w"); err == nil {
		t.Error("non-outreach job type must error")
	}
}

func TestSourceHealthExecutorGates(t *testing.T) {
	mk := func(en, sh string) ExecutorSettings {
		return ExecutorCohortFrom(func(k string) string {
			return map[string]string{"GO_EXECUTOR_ENABLED": en, "GO_EXECUTOR_SOURCE_HEALTH": sh}[k]
		})
	}
	for _, c := range []struct{ en, sh, want string }{
		{"true", "true", "go"}, {"false", "true", "python"}, {"true", "", "python"}, {"", "", "python"},
	} {
		if got := mk(c.en, c.sh).SourceHealthExecutor(); got != c.want {
			t.Errorf("enabled=%q flag=%q: got %s want %s", c.en, c.sh, got, c.want)
		}
	}
}
