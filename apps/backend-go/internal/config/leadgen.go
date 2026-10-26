package config

// LeadgenSettings are the plan 14 (leadgen collection) settings. Both Go switches default off:
// GO_COLLECTION_ROUTES_ENABLED mounts the collect/jobs/lead-utility/source-settings routes,
// GO_EXECUTOR_SOURCE_HEALTH lets the Go executor claim source_health_check jobs.
type LeadgenSettings struct {
	GoCollectionRoutesEnabled    bool // GO_COLLECTION_ROUTES_ENABLED (default false)
	GoExecutorSourceHealth       bool // GO_EXECUTOR_SOURCE_HEALTH (default false)
	CollectQueryMaxChars         int  // COLLECT_QUERY_MAX_CHARS (default 1000)
	SystemStatsRPCTimeoutSeconds int  // SYSTEM_STATS_RPC_TIMEOUT_SECONDS (default 3)
}

func loadLeadgen(get func(string) string) (LeadgenSettings, error) {
	var l LeadgenSettings
	var err error
	if l.GoCollectionRoutesEnabled, err = boolVar(get, "GO_COLLECTION_ROUTES_ENABLED", false); err != nil {
		return l, err
	}
	if l.GoExecutorSourceHealth, err = boolVar(get, "GO_EXECUTOR_SOURCE_HEALTH", false); err != nil {
		return l, err
	}
	if l.CollectQueryMaxChars, err = intVar(get, "COLLECT_QUERY_MAX_CHARS", 1000, 1, 1<<20); err != nil {
		return l, err
	}
	if l.SystemStatsRPCTimeoutSeconds, err = intVar(get, "SYSTEM_STATS_RPC_TIMEOUT_SECONDS", 3, 1, 600); err != nil {
		return l, err
	}
	return l, nil
}
