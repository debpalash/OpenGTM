package config

import (
	"strings"
	"time"
)

// AudienceSettings are the plan 10 (audiences and destinations) settings. The Go* flags are
// Go-only rollout switches, all default false; the rest keep Python's names and defaults.
type AudienceSettings struct {
	GoAudienceRoutesEnabled   bool     // GO_AUDIENCE_ROUTES_ENABLED
	GoDestRoutesEnabled       bool     // GO_DEST_ROUTES_ENABLED
	GoExecutorAudienceRefresh bool     // GO_EXECUTOR_AUDIENCE_REFRESH
	GoDestTypes               []string // GO_DEST_TYPES (comma list, lower-cased, de-duplicated)

	DestTokenTTLDays    int           // OPENGTM_DESTINATION_TOKEN_TTL_DAYS, clamped 1-365
	RefreshPageSize     int           // AUDIENCE_REFRESH_PAGE_SIZE
	DestMemberPageSize  int           // DEST_MEMBER_PAGE_SIZE
	DestAdBatchSize     int           // DEST_AD_BATCH_SIZE
	AccountsMaxMembers  int           // AUDIENCE_ACCOUNTS_MAX_MEMBERS
	DestDeliveryTimeout time.Duration // DEST_DELIVERY_TIMEOUT (seconds)
}

func loadAudiences(get func(string) string) (AudienceSettings, error) {
	var a AudienceSettings
	var err error
	for _, b := range []struct {
		dst *bool
		key string
	}{
		{&a.GoAudienceRoutesEnabled, "GO_AUDIENCE_ROUTES_ENABLED"},
		{&a.GoDestRoutesEnabled, "GO_DEST_ROUTES_ENABLED"},
		{&a.GoExecutorAudienceRefresh, "GO_EXECUTOR_AUDIENCE_REFRESH"},
	} {
		if *b.dst, err = boolVar(get, b.key, false); err != nil {
			return a, err
		}
	}
	seen := map[string]bool{}
	for _, t := range strings.Split(get("GO_DEST_TYPES"), ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" && !seen[t] {
			seen[t] = true
			a.GoDestTypes = append(a.GoDestTypes, t)
		}
	}
	if a.DestTokenTTLDays, err = clampVar(get, "OPENGTM_DESTINATION_TOKEN_TTL_DAYS", 90, 1, 365); err != nil {
		return a, err
	}
	for _, n := range []struct {
		dst *int
		key string
		def int
	}{
		{&a.RefreshPageSize, "AUDIENCE_REFRESH_PAGE_SIZE", 500},
		{&a.DestMemberPageSize, "DEST_MEMBER_PAGE_SIZE", 500},
		{&a.DestAdBatchSize, "DEST_AD_BATCH_SIZE", 5000},
		{&a.AccountsMaxMembers, "AUDIENCE_ACCOUNTS_MAX_MEMBERS", 200000},
	} {
		if *n.dst, err = intVar(get, n.key, n.def, 1, maxAutomationInt); err != nil {
			return a, err
		}
	}
	secs, err := intVar(get, "DEST_DELIVERY_TIMEOUT", 30, 1, 3600)
	if err != nil {
		return a, err
	}
	a.DestDeliveryTimeout = time.Duration(secs) * time.Second
	return a, nil
}
