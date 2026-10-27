package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestOutreachConfigDefaultsMatchPython(t *testing.T) {
	o, err := loadOutreach(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := OutreachSettings{
		PublicBaseURL: "http://localhost:8000", UnsubTTLDays: 90, TickInterval: 900, TickMaxEnqueue: 200,
		SoftBounceMax: 3, BouncePauseRate: 0.05, ComplaintPauseRate: 0.003, CircuitMinSends: 20,
		InboundPollInterval: 900, InboundMaxFetch: 100, InboundMaxConsecutiveFailures: 10, InboundLookbackDays: 3,
	}
	if o != want {
		t.Fatalf("defaults\n got %#v\nwant %#v", o, want)
	}
	if o.GoRoutesEnabled || o.GoExecutorSend || o.GoExecutorInbound || o.InboundPollEnabled || o.SendCostUSD != 0 {
		t.Fatal("rollout flags and send cost must default off/zero")
	}
}

func TestOutreachSettingsParse(t *testing.T) {
	env := map[string]string{
		"GO_OUTREACH_ROUTES_ENABLED": "true", "GO_EXECUTOR_OUTREACH_SEND": "1", "GO_EXECUTOR_OUTREACH_INBOUND": "yes",
		"OUTREACH_INBOUND_POLL_ENABLED": "on", "OUTREACH_SEND_COST_USD": "0.05", "OUTREACH_PUBLIC_BASE_URL": "https://x.example/",
		"OUTREACH_BOUNCE_WEBHOOK_SECRET": "s3cret-value", "OUTREACH_UNSUB_TTL_DAYS": "30", "OUTREACH_TICK_INTERVAL": "60",
		"OUTREACH_TICK_MAX_ENQUEUE": "5", "OUTREACH_SOFT_BOUNCE_MAX": "2", "OUTREACH_BOUNCE_PAUSE_RATE": "0.1",
		"OUTREACH_COMPLAINT_PAUSE_RATE": "0.01", "OUTREACH_CIRCUIT_MIN_SENDS": "7", "OUTREACH_INBOUND_POLL_INTERVAL": "120",
		"OUTREACH_INBOUND_MAX_FETCH": "10", "OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES": "4", "OUTREACH_INBOUND_LOOKBACK_DAYS": "1",
	}
	o, err := loadOutreach(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !o.GoRoutesEnabled || !o.GoExecutorSend || !o.GoExecutorInbound || !o.InboundPollEnabled || o.SendCostUSD != 0.05 ||
		o.PublicBaseURL != "https://x.example" || o.BounceWebhookSecret.Reveal() != "s3cret-value" || o.UnsubTTLDays != 30 ||
		o.TickInterval != 60 || o.TickMaxEnqueue != 5 || o.SoftBounceMax != 2 || o.BouncePauseRate != 0.1 ||
		o.ComplaintPauseRate != 0.01 || o.CircuitMinSends != 7 || o.InboundPollInterval != 120 || o.InboundMaxFetch != 10 ||
		o.InboundMaxConsecutiveFailures != 4 || o.InboundLookbackDays != 1 {
		t.Fatalf("parsed %#v", o)
	}
	if s := fmt.Sprintf("%v %+v %#v", o.BounceWebhookSecret, o, o); strings.Contains(s, "s3cret-value") {
		t.Fatal("webhook secret leaked through formatting")
	}
}

func TestOutreachSettingsRejectBad(t *testing.T) {
	for _, kv := range [][2]string{
		{"GO_OUTREACH_ROUTES_ENABLED", "maybe"}, {"OUTREACH_TICK_INTERVAL", "x"}, {"OUTREACH_SEND_COST_USD", "-1"},
		{"OUTREACH_BOUNCE_PAUSE_RATE", "2"}, {"OUTREACH_UNSUB_TTL_DAYS", "0"}, {"OUTREACH_TICK_MAX_ENQUEUE", "-3"},
	} {
		if _, err := loadOutreach(func(k string) string {
			if k == kv[0] {
				return kv[1]
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s accepted", kv[0], kv[1])
		}
	}
}

func TestAPIConfigCarriesOutreach(t *testing.T) {
	var f Flags
	f.Outreach.GoRoutesEnabled = true // Flags.Outreach is the plan 09 settings group
	if !f.Outreach.GoRoutesEnabled {
		t.Fatal("unreachable")
	}
}

// The worker loader must carry the outreach settings, or every go send and
// inbound poll job is handed back to Python (plan 09 final review H1).
func TestWorkerLoadCarriesOutreach(t *testing.T) {
	c, err := load(env(map[string]string{
		"INTERNAL_SERVICE_TOKEN": goodTok, "GO_EXECUTOR_OUTREACH_SEND": "true", "GO_EXECUTOR_OUTREACH_INBOUND": "true",
		"OUTREACH_INBOUND_POLL_ENABLED": "true", "OUTREACH_PUBLIC_BASE_URL": "https://x.example", "OUTREACH_UNSUB_TTL_DAYS": "30",
	}))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Flags.Outreach
	if !o.GoExecutorSend || !o.GoExecutorInbound || !o.InboundPollEnabled || o.PublicBaseURL != "https://x.example" ||
		o.UnsubTTLDays != 30 || o.CircuitMinSends != 20 || o.BouncePauseRate != 0.05 || o.ComplaintPauseRate != 0.003 {
		t.Fatalf("worker config dropped outreach settings: %#v", o)
	}
	if _, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "OUTREACH_UNSUB_TTL_DAYS": "0"})); err == nil {
		t.Fatal("bad OUTREACH_* value accepted by the worker loader")
	}
}

// S-L17: with outreach routes or sending on, a non-dev environment needs an https public base URL.
func TestOutreachPublicBaseURLRequiredOutsideDev(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"routes on, prod, unset", map[string]string{"GO_OUTREACH_ROUTES_ENABLED": "1", "APP_ENV": "production"}, false},
		{"routes on, APP_ENV unset, unset url", map[string]string{"GO_OUTREACH_ROUTES_ENABLED": "1"}, false},
		{"send on, prod, http", map[string]string{"GO_EXECUTOR_OUTREACH_SEND": "1", "APP_ENV": "production", "OUTREACH_PUBLIC_BASE_URL": "http://app.example.com"}, false},
		{"routes on, prod, https", map[string]string{"GO_OUTREACH_ROUTES_ENABLED": "1", "APP_ENV": "production", "OUTREACH_PUBLIC_BASE_URL": "https://app.example.com/"}, true},
		{"routes on, dev, unset", map[string]string{"GO_OUTREACH_ROUTES_ENABLED": "1", "APP_ENV": "dev"}, true},
		{"flags off, prod, unset", map[string]string{"APP_ENV": "production"}, true},
		{"inbound only, prod, unset", map[string]string{"GO_EXECUTOR_OUTREACH_INBOUND": "1", "APP_ENV": "production"}, true},
	} {
		_, err := loadOutreach(func(k string) string { return c.env[k] })
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}
