package config

import (
	"fmt"
)

// ExecutorSettings are the scan caps the worker reads for refresh_workbook and signal_scan, plus the routing
// fields of the former go_executor.py cohort. Since plan 15 WP-CUTOVER every job runs on the Go worker: the
// routing flags (GO_EXECUTOR_ENABLED, GO_EXECUTOR_WORKSPACES, GO_EXECUTOR_SIGNAL_SCAN, GO_EXECUTOR_SOURCE_HEALTH)
// are permanently on and no longer read, and every resolver below names "go".
type ExecutorSettings struct {
	Enabled      bool            // always true (GO_EXECUTOR_ENABLED is retired)
	Workspaces   map[string]bool // always {"*"}: every workspace (GO_EXECUTOR_WORKSPACES is retired)
	SignalScan   bool            // always true (GO_EXECUTOR_SIGNAL_SCAN is retired)
	SourceHealth bool            // always true (GO_EXECUTOR_SOURCE_HEALTH is retired)

	SignalHotLeads, SignalWarmLeads, SignalPerWorkspace, SignalGlobal int // SIGNAL_SCAN_MAX_HOT_LEADS, _MAX_WARM_LEADS, _MAX_LEADS_PER_WORKSPACE, _GLOBAL_MAX_LEADS
}

// pyBool is pydantic's lenient bool for the Python-named flags.
func pyBool(s string) bool { return truthyValue(s) }

// ExecutorGo is the executor of every job since the cutover.
const ExecutorGo = "go"

// ExecutorCohortFrom returns the (permanently on) routing settings; get is no longer consulted.
func ExecutorCohortFrom(func(string) string) ExecutorSettings {
	return ExecutorSettings{Enabled: true, SignalScan: true, SourceHealth: true, Workspaces: map[string]bool{"*": true}}
}

func loadExecutor(get func(string) string) (ExecutorSettings, error) {
	e := ExecutorCohortFrom(get)
	var err error
	for _, n := range []struct {
		dst *int
		key string
		def int
	}{
		{&e.SignalHotLeads, "SIGNAL_SCAN_MAX_HOT_LEADS", 50},
		{&e.SignalWarmLeads, "SIGNAL_SCAN_MAX_WARM_LEADS", 30},
		{&e.SignalPerWorkspace, "SIGNAL_SCAN_MAX_LEADS_PER_WORKSPACE", 20},
		{&e.SignalGlobal, "SIGNAL_SCAN_GLOBAL_MAX_LEADS", 500}, // 0 = unlimited
	} {
		if *n.dst, err = intVar(get, n.key, n.def, 0, maxAutomationInt); err != nil {
			return e, err
		}
	}
	return e, nil
}

// RefreshExecutor is the executor of a refresh_workbook / signal_scan follow-up: always "go".
func (e ExecutorSettings) RefreshExecutor(string) string { return ExecutorGo }

// SignalScanExecutor is the executor of a signal_scan job: always "go".
func (e ExecutorSettings) SignalScanExecutor() string { return ExecutorGo }

// OutreachExecutor is the executor of a send / outreach_inbound_poll job: always "go". Other job types are an
// error, as in Python.
func (e ExecutorSettings) OutreachExecutor(_ OutreachSettings, jobType, _ string) (string, error) {
	switch jobType {
	case "send", "outreach_inbound_poll":
		return ExecutorGo, nil
	}
	return "", fmt.Errorf("config: not an outreach executor job type: %q", jobType)
}

// SourceHealthExecutor is the executor of a source_health_check job: always "go".
func (e ExecutorSettings) SourceHealthExecutor() string { return ExecutorGo }
