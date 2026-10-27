package config

import (
	"os"
	"strings"
)

// GO_ALL_NATIVE is the plan 15 master switch (WP-ALLGO): with it true, every Go-native flag in NativeFlags that the
// operator did not set explicitly resolves to its "on" value, so "everything in Go" is testable before WP-CUTOVER
// flips the individual defaults. A flag set to any non-empty value (including false) keeps that value. With the
// switch unset or false nothing changes, except for the retired flags (RetiredFlags): since WP-CUTOVER those are
// permanently on whatever the environment says.
//
// How flag readers consult it: readers take a getter (func(string) string). Entry points get their getter from
// Getenv (or wrap their own source with AllNative), so every existing reader sees resolved values without per-flag
// code. A package that adds a new GO_* flag must add it to NativeFlags below (TestRegistryCoversEverySourceFlag
// fails otherwise) and read it through a config getter or config.Getenv, never os.Getenv directly. Flags whose
// package has not landed yet already have an entry, so the package picks the switch up for free.

// NativeFlag is one registry entry: the env key and the value the master switch gives it when unset.
type NativeFlag struct {
	Key string
	On  string
}

const allNativeKey = "GO_ALL_NATIVE"

// goDestTypesAll is every destination type (destvalidate). Since the cutover the worker runs every
// audience_destination_sync job; a type without a Go delivery port fails permanently there.
const goDestTypesAll = "webhook,slack,hubspot,salesforce,instantly,smartlead,google_sheets,airtable,meta_ads,google_ads,linkedin_ads,warehouse_http"

func on(keys ...string) []NativeFlag {
	out := make([]NativeFlag, len(keys))
	for i, k := range keys {
		out[i] = NativeFlag{k, "true"}
	}
	return out
}

var nativeFlags = append(on(
	// jobs / executors
	"GO_EXECUTOR_ENABLED", "GO_EXECUTOR_FULL_RUN", "GO_EXECUTOR_SIGNAL_SCAN",
	"GO_EXECUTOR_SOURCE_HEALTH", "GO_EXECUTOR_AUDIENCE_REFRESH", "GO_EXECUTOR_OUTREACH_SEND",
	"GO_EXECUTOR_OUTREACH_INBOUND", "GO_EXECUTOR_PLAYBOOKS", "GO_EXECUTOR_RETENTION", "GO_EXECUTOR_TRIGGER_EVAL",
	"GO_EXECUTOR_WATCH_POLL", "GO_EXECUTOR_COLLECT", "GO_EXECUTOR_BULK_ENRICH",
	"GO_RUN_NATIVE_PROVIDERS", "GO_SCHEDULER_ENABLED",
	// routes
	"GO_AUDIENCE_ROUTES_ENABLED", "GO_AUTOMATION_ROUTES_ENABLED", "GO_CAPABILITY_ROUTES_ENABLED", "GO_COLLECTION_ROUTES_ENABLED",
	"GO_CONNECTOR_ROUTES_ENABLED", "GO_DEST_ROUTES_ENABLED", "GO_GOVERNANCE_ROUTES_ENABLED", "GO_LLM_ROUTES_ENABLED",
	"GO_OUTREACH_ROUTES_ENABLED", "GO_PLAYBOOK_ROUTES_ENABLED", "GO_RUN_ROUTES_ENABLED", "GO_SCRAPE_ROUTES_ENABLED",
	"GO_SOURCE_PREVIEW_ENABLED", "GO_TRAILING_SLASH_COMPAT",
	// native subsystems
	"GO_COPILOT_NATIVE", "GO_NATIVE_AI_COLUMNS", "GO_NATIVE_SOURCE_PROBE",
), []NativeFlag{
	{"GO_EXECUTOR_WORKSPACES", "*"}, // cohort = every workspace; read it through InCohort
	{"GO_LOCAL_PROVIDERS", "all"},
	{"GO_EXECUTOR_COLUMN_TYPES", "formula,http,ai_formula,research,agent"},
	{"GO_DEST_TYPES", goDestTypesAll},
}...)

// NativeFlags returns the registry (a copy).
func NativeFlags() []NativeFlag { return append([]NativeFlag(nil), nativeFlags...) }

// retiredKeys are the flags WP-CUTOVER (plan 15) made permanently on: the job executor routing and the native
// subsystems whose "off" meant "Python runs it". With Python gone there is no off: AllNative (and so Getenv and every
// config loader) resolves each to its on-value whatever the environment or GO_ALL_NATIVE say, and an explicit false is
// ignored. The keys stay readable so callers that still consult them (internal/api until its own cutover) see "on".
var retiredKeys = map[string]bool{
	"GO_EXECUTOR_ENABLED": true, "GO_EXECUTOR_WORKSPACES": true, "GO_EXECUTOR_FULL_RUN": true,
	"GO_EXECUTOR_SIGNAL_SCAN": true, "GO_EXECUTOR_SOURCE_HEALTH": true,
	"GO_EXECUTOR_AUDIENCE_REFRESH": true, "GO_EXECUTOR_OUTREACH_SEND": true,
	"GO_EXECUTOR_OUTREACH_INBOUND": true, "GO_EXECUTOR_PLAYBOOKS": true, "GO_EXECUTOR_RETENTION": true,
	"GO_EXECUTOR_TRIGGER_EVAL": true, "GO_EXECUTOR_WATCH_POLL": true, "GO_EXECUTOR_COLLECT": true,
	"GO_EXECUTOR_BULK_ENRICH": true, "GO_EXECUTOR_COLUMN_TYPES": true,
	"GO_RUN_NATIVE_PROVIDERS": true, "GO_LOCAL_PROVIDERS": true, "GO_NATIVE_AI_COLUMNS": true,
	"GO_NATIVE_SOURCE_PROBE": true, "GO_DEST_TYPES": true,
}

func isRetired(key string) bool { return retiredKeys[key] }

// RetiredFlags returns the permanently-on flags with their fixed value.
func RetiredFlags() []NativeFlag {
	var out []NativeFlag
	for _, f := range nativeFlags {
		if retiredKeys[f.Key] {
			out = append(out, f)
		}
	}
	return out
}

func truthyValue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "t", "y":
		return true
	}
	return false
}

// AllNativeEnabled reports whether GO_ALL_NATIVE is set to a true spelling in get.
func AllNativeEnabled(get func(string) string) bool { return truthyValue(get(allNativeKey)) }

// AllNative wraps get so registry flags that are unset resolve to their "on" value when GO_ALL_NATIVE is true.
// Retired flags (RetiredFlags) always resolve to their on-value. Other keys, and every registry flag while the
// switch is off, return get's values untouched.
func AllNative(get func(string) string) func(string) string {
	all := AllNativeEnabled(get)
	defaults := make(map[string]string, len(nativeFlags))
	for _, f := range nativeFlags {
		defaults[f.Key] = f.On
	}
	return func(key string) string {
		if retiredKeys[key] {
			return defaults[key]
		}
		v := get(key)
		if !all || strings.TrimSpace(v) != "" {
			return v
		}
		if d, ok := defaults[key]; ok {
			return d
		}
		return v
	}
}

// Getenv is os.Getenv resolved through the master switch; entry points use it as their flag source.
func Getenv(key string) string { return AllNative(os.Getenv)(key) }

// InCohort reports whether workspace ws is in a GO_EXECUTOR_WORKSPACES set; "*" (the master switch) means all.
func InCohort(cohort map[string]bool, ws string) bool { return cohort["*"] || cohort[ws] }
