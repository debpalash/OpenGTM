package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CollectSettings are the plan 15 WP-COLLECT switches and the leadgen collection knobs.
//
// GO_EXECUTOR_COLLECT / GO_EXECUTOR_BULK_ENRICH (default false = Python): collect and
// bulk_enrich jobs are enqueued with executor 'go' for workspaces in the GO_EXECUTOR_ENABLED +
// GO_EXECUTOR_WORKSPACES cohort, and the Go worker runs them (it hands others back).
// GO_NATIVE_SOURCE_PROBE (default false): source_health_check probes the registry sources in
// process (internal/collect over leadweb) instead of the Python worker's source-list /
// source-probe RPCs.
//
// The knobs keep the legacy names and defaults (job_runner.py os.getenv reads, the source
// health gate flag); unlike legacy, a malformed number is a startup error.
type CollectSettings struct {
	GoCollect     bool // GO_EXECUTOR_COLLECT
	GoBulkEnrich  bool // GO_EXECUTOR_BULK_ENRICH
	NativeProbe   bool // GO_NATIVE_SOURCE_PROBE
	HealthEnforce bool // SOURCE_HEALTH_ENFORCE (the live-job health gate)

	RegistrySourceCap    int           // REGISTRY_SOURCE_CAP (20)
	DMBudget             int           // DM_DISCOVERY_BUDGET (15)
	EmailWaterfallBudget int           // EMAIL_WATERFALL_BUDGET (15)
	DeliverabilityBudget int           // DELIVERABILITY_BUDGET (20)
	SourceReliability    bool          // SOURCE_RELIABILITY_RANKING (false)
	SearchTimeout        time.Duration // DDG_SEARCH_TIMEOUT seconds (25)
}

// LoadCollect reads the settings strictly.
func LoadCollect(get func(string) string) (CollectSettings, error) {
	s := CollectSettings{RegistrySourceCap: 20, DMBudget: 15, EmailWaterfallBudget: 15, DeliverabilityBudget: 20,
		SearchTimeout: 25 * time.Second}
	for _, f := range []struct {
		dst *bool
		key string
	}{
		{&s.GoCollect, "GO_EXECUTOR_COLLECT"},
		{&s.GoBulkEnrich, "GO_EXECUTOR_BULK_ENRICH"},
		{&s.NativeProbe, "GO_NATIVE_SOURCE_PROBE"},
		{&s.HealthEnforce, "SOURCE_HEALTH_ENFORCE"},
		{&s.SourceReliability, "SOURCE_RELIABILITY_RANKING"},
	} {
		v, err := boolVar(get, f.key, false)
		if err != nil {
			return s, err
		}
		*f.dst = v
	}
	for _, f := range []struct {
		dst *int
		key string
	}{
		{&s.RegistrySourceCap, "REGISTRY_SOURCE_CAP"},
		{&s.DMBudget, "DM_DISCOVERY_BUDGET"},
		{&s.EmailWaterfallBudget, "EMAIL_WATERFALL_BUDGET"},
		{&s.DeliverabilityBudget, "DELIVERABILITY_BUDGET"},
	} {
		if v := strings.TrimSpace(get(f.key)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return s, fmt.Errorf("config: %s=%q: not an integer", f.key, v)
			}
			*f.dst = n
		}
	}
	if v := strings.TrimSpace(get("DDG_SEARCH_TIMEOUT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return s, fmt.Errorf("config: DDG_SEARCH_TIMEOUT=%q: not a positive integer", v)
		}
		s.SearchTimeout = time.Duration(n) * time.Second
	}
	return s, nil
}

// CollectExecutor is the executor of a new collect job: "go" only when GO_EXECUTOR_ENABLED,
// GO_EXECUTOR_COLLECT and cohort membership (GO_EXECUTOR_WORKSPACES; "*" = all) all hold, else
// "python" (today's behaviour). Lenient (pydantic bool) so request handlers can resolve it per call.
func CollectExecutor(get func(string) string, workspaceID string) string {
	return cohortExecutor(get, "GO_EXECUTOR_COLLECT", workspaceID)
}

// BulkEnrichExecutor is CollectExecutor for bulk_enrich (GO_EXECUTOR_BULK_ENRICH).
func BulkEnrichExecutor(get func(string) string, workspaceID string) string {
	return cohortExecutor(get, "GO_EXECUTOR_BULK_ENRICH", workspaceID)
}

func cohortExecutor(get func(string) string, kind, ws string) string {
	e := ExecutorCohortFrom(get)
	if e.Enabled && pyBool(get(kind)) && InCohort(e.Workspaces, ws) {
		return "go"
	}
	return "python"
}
