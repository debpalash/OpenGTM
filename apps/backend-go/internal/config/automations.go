package config

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// AutomationsSettings are the plan 08 (automations and intent poller) settings. The
// Python-named ones keep their Python names and defaults (core/config.py); the Go*
// flags are Go-only rollout switches, all default false. AUTOMATIONS_ENABLED,
// INTENT_POLLER_ENABLED and AUTOMATIONS_ALLOW_LEGACY_OUTREACH live on Flags.
type AutomationsSettings struct {
	GoRoutesEnabled       bool // GO_AUTOMATION_ROUTES_ENABLED
	GoExecutorTriggerEval bool // GO_EXECUTOR_TRIGGER_EVAL
	GoExecutorWatchPoll   bool // GO_EXECUTOR_WATCH_POLL
	// GoExecutorLegacyOutreachActions (GO_EXECUTOR_LEGACY_OUTREACH_ACTIONS) lets Go run
	// the sequencer and send_email trigger actions instead of handing the eval back.
	GoExecutorLegacyOutreachActions bool

	GlobalDailyUSD         float64  // AUTOMATIONS_GLOBAL_DAILY_USD (0 = unlimited)
	MaxRulesPerWS          int      // AUTOMATIONS_MAX_RULES_PER_WS
	MaxActionsPerRule      int      // AUTOMATIONS_MAX_ACTIONS_PER_RULE
	MaxRowsPerEval         int      // AUTOMATIONS_MAX_ROWS_PER_EVAL
	WebhookDomainAllowlist []string // AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST (JSON array or comma list)

	PollerDefaultInterval             string  // INTENT_POLLER_DEFAULT_INTERVAL: daily|hourly|weekly
	PollerMaxWatchesPerWS             int     // INTENT_POLLER_MAX_WATCHES_PER_WS
	PollerDailyPollBudget             int     // INTENT_POLLER_DAILY_POLL_BUDGET (0 = unlimited)
	PollerPollNowDailyQuota           int     // INTENT_POLLER_POLL_NOW_DAILY_QUOTA
	PollerPollNowMinIntervalSec       int     // INTENT_POLLER_POLL_NOW_MIN_INTERVAL_SEC
	PollerMaxConsecutiveFailures      int     // INTENT_POLLER_MAX_CONSECUTIVE_FAILURES
	PollerJobspyMaxJobs               int     // INTENT_POLLER_JOBSPY_MAX_JOBS
	PollerFeedMaxEntries              int     // INTENT_POLLER_FEED_MAX_ENTRIES
	PollerJobChangeMaxContactsPerPoll int     // INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS_PER_POLL
	PollerJobChangeMaxContacts        int     // INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS
	PollerMaxFiresPerSignal           int     // INTENT_POLLER_MAX_FIRES_PER_SIGNAL
	PollerBackfill                    bool    // INTENT_POLLER_BACKFILL
	PollerFundingCostUSD              float64 // POLLER_FUNDING_COST_USD
	PollerHiringCostUSD               float64 // POLLER_HIRING_COST_USD
	PollerFeedCostUSD                 float64 // POLLER_FEED_COST_USD

	TechStackWebsiteFetchEnabled bool // TECH_STACK_WEBSITE_FETCH_ENABLED
}

const maxAutomationInt = 1 << 30

func loadAutomations(get func(string) string) (AutomationsSettings, error) {
	var a AutomationsSettings
	var err error
	for _, b := range []struct {
		dst *bool
		key string
	}{
		{&a.GoRoutesEnabled, "GO_AUTOMATION_ROUTES_ENABLED"},
		{&a.GoExecutorTriggerEval, "GO_EXECUTOR_TRIGGER_EVAL"},
		{&a.GoExecutorWatchPoll, "GO_EXECUTOR_WATCH_POLL"},
		{&a.GoExecutorLegacyOutreachActions, "GO_EXECUTOR_LEGACY_OUTREACH_ACTIONS"},
		{&a.PollerBackfill, "INTENT_POLLER_BACKFILL"},
		{&a.TechStackWebsiteFetchEnabled, "TECH_STACK_WEBSITE_FETCH_ENABLED"},
	} {
		if *b.dst, err = boolVar(get, b.key, false); err != nil {
			return a, err
		}
	}
	for _, n := range []struct {
		dst      *int
		key      string
		def, min int
	}{
		{&a.MaxRulesPerWS, "AUTOMATIONS_MAX_RULES_PER_WS", 50, 1},
		{&a.MaxActionsPerRule, "AUTOMATIONS_MAX_ACTIONS_PER_RULE", 10, 1},
		{&a.MaxRowsPerEval, "AUTOMATIONS_MAX_ROWS_PER_EVAL", 500, 1},
		{&a.PollerMaxWatchesPerWS, "INTENT_POLLER_MAX_WATCHES_PER_WS", 200, 1},
		{&a.PollerDailyPollBudget, "INTENT_POLLER_DAILY_POLL_BUDGET", 0, 0},
		{&a.PollerPollNowDailyQuota, "INTENT_POLLER_POLL_NOW_DAILY_QUOTA", 50, 0},
		{&a.PollerPollNowMinIntervalSec, "INTENT_POLLER_POLL_NOW_MIN_INTERVAL_SEC", 60, 0},
		{&a.PollerMaxConsecutiveFailures, "INTENT_POLLER_MAX_CONSECUTIVE_FAILURES", 12, 1},
		{&a.PollerJobspyMaxJobs, "INTENT_POLLER_JOBSPY_MAX_JOBS", 5, 1},
		{&a.PollerFeedMaxEntries, "INTENT_POLLER_FEED_MAX_ENTRIES", 100, 1},
		{&a.PollerJobChangeMaxContactsPerPoll, "INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS_PER_POLL", 50, 1},
		{&a.PollerJobChangeMaxContacts, "INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS", 500, 1},
		{&a.PollerMaxFiresPerSignal, "INTENT_POLLER_MAX_FIRES_PER_SIGNAL", 200, 1},
	} {
		if *n.dst, err = intVar(get, n.key, n.def, n.min, maxAutomationInt); err != nil {
			return a, err
		}
	}
	for _, f := range []struct {
		dst *float64
		key string
	}{
		{&a.GlobalDailyUSD, "AUTOMATIONS_GLOBAL_DAILY_USD"},
		{&a.PollerFundingCostUSD, "POLLER_FUNDING_COST_USD"},
		{&a.PollerHiringCostUSD, "POLLER_HIRING_COST_USD"},
		{&a.PollerFeedCostUSD, "POLLER_FEED_COST_USD"},
	} {
		if *f.dst, err = usdVar(get, f.key); err != nil {
			return a, err
		}
	}
	a.PollerDefaultInterval = "daily"
	if v := strings.ToLower(strings.TrimSpace(get("INTENT_POLLER_DEFAULT_INTERVAL"))); v != "" {
		switch v {
		case "daily", "hourly", "weekly":
			a.PollerDefaultInterval = v
		default:
			return a, fmt.Errorf("config: INTENT_POLLER_DEFAULT_INTERVAL must be daily, hourly or weekly")
		}
	}
	a.WebhookDomainAllowlist = parseStringList(get("AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST"))
	return a, nil
}

// usdVar reads a non-negative, finite USD amount (default 0).
func usdVar(get func(string) string, key string) (float64, error) {
	s := strings.TrimSpace(get(key))
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fmt.Errorf("config: %s must be a non-negative number", key)
	}
	return f, nil
}

// parseStringList accepts a JSON array (the pydantic env form) or a comma list;
// entries are trimmed and empties dropped. Nothing set yields nil.
func parseStringList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var raw []string
	if strings.HasPrefix(s, "[") {
		if json.Unmarshal([]byte(s), &raw) != nil {
			raw = strings.Split(strings.Trim(s, "[]"), ",")
		}
	} else {
		raw = strings.Split(s, ",")
	}
	var out []string
	for _, v := range raw {
		if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
			out = append(out, v)
		}
	}
	return out
}
