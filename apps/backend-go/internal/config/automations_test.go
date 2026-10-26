package config

import (
	"reflect"
	"testing"
)

func TestConfigDefaultsMatchPython(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test"}))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Flags.Automations
	want := AutomationsSettings{
		GoRoutesEnabled: false, GoExecutorTriggerEval: false, GoExecutorWatchPoll: false,
		GlobalDailyUSD: 0, MaxRulesPerWS: 50, MaxActionsPerRule: 10, MaxRowsPerEval: 500,
		WebhookDomainAllowlist:            nil,
		PollerDefaultInterval:             "daily",
		PollerMaxWatchesPerWS:             200,
		PollerDailyPollBudget:             0,
		PollerPollNowDailyQuota:           50,
		PollerPollNowMinIntervalSec:       60,
		PollerMaxConsecutiveFailures:      12,
		PollerJobspyMaxJobs:               5,
		PollerFeedMaxEntries:              100,
		PollerJobChangeMaxContactsPerPoll: 50,
		PollerJobChangeMaxContacts:        500,
		PollerMaxFiresPerSignal:           200,
		PollerBackfill:                    false,
		PollerFundingCostUSD:              0, PollerHiringCostUSD: 0, PollerFeedCostUSD: 0,
		TechStackWebsiteFetchEnabled: false,
	}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("defaults differ:\n got %+v\nwant %+v", a, want)
	}
}

func TestConfigAutomationsEnvOverrides(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test",
		"GO_AUTOMATION_ROUTES_ENABLED": "true", "GO_EXECUTOR_TRIGGER_EVAL": "1", "GO_EXECUTOR_WATCH_POLL": "yes",
		"AUTOMATIONS_GLOBAL_DAILY_USD": "2.5", "AUTOMATIONS_MAX_RULES_PER_WS": "7",
		"AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST": `["a.example"," b.example "]`,
		"INTENT_POLLER_DEFAULT_INTERVAL":       " Hourly ", "INTENT_POLLER_BACKFILL": "true",
		"POLLER_FEED_COST_USD": "0.01", "TECH_STACK_WEBSITE_FETCH_ENABLED": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Flags.Automations
	if !a.GoRoutesEnabled || !a.GoExecutorTriggerEval || !a.GoExecutorWatchPoll || a.GlobalDailyUSD != 2.5 ||
		a.MaxRulesPerWS != 7 || a.PollerDefaultInterval != "hourly" || !a.PollerBackfill ||
		a.PollerFeedCostUSD != 0.01 || !a.TechStackWebsiteFetchEnabled ||
		!reflect.DeepEqual(a.WebhookDomainAllowlist, []string{"a.example", "b.example"}) {
		t.Fatalf("overrides not read: %+v", a)
	}
	c, err = loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST": "x.io, y.io"}))
	if err != nil || !reflect.DeepEqual(c.Flags.Automations.WebhookDomainAllowlist, []string{"x.io", "y.io"}) {
		t.Fatalf("csv allowlist: %v %+v", err, c.Flags.Automations.WebhookDomainAllowlist)
	}
}

func TestConfigRejectsBadInterval(t *testing.T) {
	for _, v := range []string{"monthly", "5m", "dayly"} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "INTENT_POLLER_DEFAULT_INTERVAL": v})); err == nil {
			t.Errorf("interval %q accepted", v)
		}
	}
	for _, e := range []map[string]string{
		{"AUTOMATIONS_MAX_RULES_PER_WS": "abc"}, {"AUTOMATIONS_GLOBAL_DAILY_USD": "-1"},
		{"AUTOMATIONS_GLOBAL_DAILY_USD": "NaN"}, {"POLLER_HIRING_COST_USD": "-0.5"},
		{"INTENT_POLLER_BACKFILL": "maybe"}, {"GO_EXECUTOR_WATCH_POLL": "x"},
	} {
		e["APP_ENV"] = "test"
		if _, err := loadAPI(apiEnv(e)); err == nil {
			t.Errorf("bad env %v accepted", e)
		}
	}
}

// The worker Config carries the plan 08 settings (Flags.Automations and the flags the executors read).
func TestWorkerLoadsAutomationSettings(t *testing.T) {
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok}))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Flags.Automations
	if a.MaxRowsPerEval != 500 || a.PollerDefaultInterval != "daily" || a.GoExecutorTriggerEval || a.GoExecutorWatchPoll ||
		c.Flags.IntentPollerEnabled || !c.Flags.PGLeadStore || c.Flags.AutomationsAllowLegacyOutreach || c.BillingEnabled {
		t.Fatalf("defaults: %+v billing=%v", c.Flags, c.BillingEnabled)
	}
	c, err = load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "GO_EXECUTOR_TRIGGER_EVAL": "true", "GO_EXECUTOR_WATCH_POLL": "1",
		"AUTOMATIONS_MAX_ROWS_PER_EVAL": "7", "INTENT_POLLER_ENABLED": "true", "PG_LEAD_STORE": "false", "BILLING_ENABLED": "true",
		"AUTOMATIONS_ALLOW_LEGACY_OUTREACH": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	a = c.Flags.Automations
	if !a.GoExecutorTriggerEval || !a.GoExecutorWatchPoll || a.MaxRowsPerEval != 7 || !c.Flags.IntentPollerEnabled ||
		c.Flags.PGLeadStore || !c.BillingEnabled || !c.Flags.AutomationsAllowLegacyOutreach {
		t.Fatalf("set: %+v", c.Flags)
	}
	for _, k := range []string{"GO_EXECUTOR_WATCH_POLL", "INTENT_POLLER_ENABLED", "BILLING_ENABLED", "AUTOMATIONS_MAX_ROWS_PER_EVAL"} {
		if _, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, k: "maybe"})); err == nil {
			t.Errorf("%s: invalid value must be rejected", k)
		}
	}
}
