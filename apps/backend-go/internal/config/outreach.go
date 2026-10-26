package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// OutreachSettings are the plan 09 (outreach) settings. Python-named ones keep their
// names and defaults (apps/api/core/config.py); the Go* flags are Go-only rollout
// switches, all default false. AUTOMATIONS_ENABLED lives on Flags.
type OutreachSettings struct {
	GoRoutesEnabled   bool // GO_OUTREACH_ROUTES_ENABLED
	GoExecutorSend    bool // GO_EXECUTOR_OUTREACH_SEND
	GoExecutorInbound bool // GO_EXECUTOR_OUTREACH_INBOUND

	PublicBaseURL       string  // OUTREACH_PUBLIC_BASE_URL (no trailing slash)
	BounceWebhookSecret Secret  // OUTREACH_BOUNCE_WEBHOOK_SECRET (empty: webhook answers 503)
	UnsubTTLDays        int     // OUTREACH_UNSUB_TTL_DAYS
	SendCostUSD         float64 // OUTREACH_SEND_COST_USD

	TickInterval       int     // OUTREACH_TICK_INTERVAL seconds
	TickMaxEnqueue     int     // OUTREACH_TICK_MAX_ENQUEUE
	SoftBounceMax      int     // OUTREACH_SOFT_BOUNCE_MAX
	BouncePauseRate    float64 // OUTREACH_BOUNCE_PAUSE_RATE
	ComplaintPauseRate float64 // OUTREACH_COMPLAINT_PAUSE_RATE
	CircuitMinSends    int     // OUTREACH_CIRCUIT_MIN_SENDS

	InboundPollEnabled            bool // OUTREACH_INBOUND_POLL_ENABLED
	InboundPollInterval           int  // OUTREACH_INBOUND_POLL_INTERVAL seconds
	InboundMaxFetch               int  // OUTREACH_INBOUND_MAX_FETCH
	InboundMaxConsecutiveFailures int  // OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES
	InboundLookbackDays           int  // OUTREACH_INBOUND_LOOKBACK_DAYS
}

func loadOutreach(get func(string) string) (OutreachSettings, error) {
	var o OutreachSettings
	var err error
	for _, b := range []struct {
		dst *bool
		key string
	}{
		{&o.GoRoutesEnabled, "GO_OUTREACH_ROUTES_ENABLED"},
		{&o.GoExecutorSend, "GO_EXECUTOR_OUTREACH_SEND"},
		{&o.GoExecutorInbound, "GO_EXECUTOR_OUTREACH_INBOUND"},
		{&o.InboundPollEnabled, "OUTREACH_INBOUND_POLL_ENABLED"},
	} {
		if *b.dst, err = boolVar(get, b.key, false); err != nil {
			return o, err
		}
	}
	for _, n := range []struct {
		dst      *int
		key      string
		def, min int
	}{
		{&o.UnsubTTLDays, "OUTREACH_UNSUB_TTL_DAYS", 90, 1},
		{&o.TickInterval, "OUTREACH_TICK_INTERVAL", 900, 1},
		{&o.TickMaxEnqueue, "OUTREACH_TICK_MAX_ENQUEUE", 200, 0},
		{&o.SoftBounceMax, "OUTREACH_SOFT_BOUNCE_MAX", 3, 0},
		{&o.CircuitMinSends, "OUTREACH_CIRCUIT_MIN_SENDS", 20, 0},
		{&o.InboundPollInterval, "OUTREACH_INBOUND_POLL_INTERVAL", 900, 1},
		{&o.InboundMaxFetch, "OUTREACH_INBOUND_MAX_FETCH", 100, 1},
		{&o.InboundMaxConsecutiveFailures, "OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES", 10, 1},
		{&o.InboundLookbackDays, "OUTREACH_INBOUND_LOOKBACK_DAYS", 3, 0},
	} {
		if *n.dst, err = intVar(get, n.key, n.def, n.min, maxAutomationInt); err != nil {
			return o, err
		}
	}
	if o.SendCostUSD, err = usdVar(get, "OUTREACH_SEND_COST_USD"); err != nil {
		return o, err
	}
	o.BouncePauseRate, o.ComplaintPauseRate = 0.05, 0.003
	for _, r := range []struct {
		dst *float64
		key string
	}{
		{&o.BouncePauseRate, "OUTREACH_BOUNCE_PAUSE_RATE"},
		{&o.ComplaintPauseRate, "OUTREACH_COMPLAINT_PAUSE_RATE"},
	} {
		if s := strings.TrimSpace(get(r.key)); s != "" {
			f, perr := strconv.ParseFloat(s, 64)
			if perr != nil || math.IsNaN(f) || f < 0 || f > 1 {
				return o, fmt.Errorf("config: %s must be a rate between 0 and 1", r.key)
			}
			*r.dst = f
		}
	}
	o.PublicBaseURL = strings.TrimRight(strings.TrimSpace(get("OUTREACH_PUBLIC_BASE_URL")), "/")
	if o.PublicBaseURL == "" {
		o.PublicBaseURL = "http://localhost:8000"
	}
	o.BounceWebhookSecret = Secret(strings.TrimSpace(get("OUTREACH_BOUNCE_WEBHOOK_SECRET")))
	return o, nil
}
