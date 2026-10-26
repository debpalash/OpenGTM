package config

import (
	"fmt"
	"strings"
)

// ExecutorSettings are the go_executor.py cohort flags and the scan caps the
// worker reads for refresh_workbook and signal_scan, plus the Python worker RPC
// endpoint. Names and defaults follow core/config.py.
type ExecutorSettings struct {
	Enabled    bool            // GO_EXECUTOR_ENABLED
	Workspaces map[string]bool // GO_EXECUTOR_WORKSPACES (comma separated)
	SignalScan bool            // GO_EXECUTOR_SIGNAL_SCAN

	SignalHotLeads, SignalWarmLeads, SignalPerWorkspace, SignalGlobal int // SIGNAL_SCAN_MAX_HOT_LEADS, _MAX_WARM_LEADS, _MAX_LEADS_PER_WORKSPACE, _GLOBAL_MAX_LEADS

	PyWorkerURL   string // PY_WORKER_URL (empty: Python-only columns are handed back)
	PyWorkerToken string // PY_WORKER_TOKEN, default INTERNAL_SERVICE_TOKEN
}

// pyBool is pydantic's lenient bool for the Python-named flags.
func pyBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "t", "y":
		return true
	}
	return false
}

// ExecutorCohortFrom reads only the routing flags (GO_EXECUTOR_ENABLED,
// GO_EXECUTOR_WORKSPACES, GO_EXECUTOR_SIGNAL_SCAN); it cannot fail, so request
// handlers can resolve a job's executor from the environment per call.
func ExecutorCohortFrom(get func(string) string) ExecutorSettings {
	e := ExecutorSettings{
		Enabled:    pyBool(get("GO_EXECUTOR_ENABLED")),
		SignalScan: pyBool(get("GO_EXECUTOR_SIGNAL_SCAN")),
		Workspaces: map[string]bool{},
	}
	for _, w := range strings.Split(get("GO_EXECUTOR_WORKSPACES"), ",") {
		if w = strings.TrimSpace(w); w != "" {
			e.Workspaces[w] = true
		}
	}
	return e
}

func loadExecutor(get func(string) string, serviceToken string) (ExecutorSettings, error) {
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
	e.PyWorkerURL = strings.TrimSpace(get("PY_WORKER_URL"))
	e.PyWorkerToken = strings.TrimSpace(get("PY_WORKER_TOKEN"))
	if e.PyWorkerToken == "" { // the Python endpoint authenticates with INTERNAL_SERVICE_TOKEN
		e.PyWorkerToken = serviceToken
	}
	return e, nil
}

// RefreshExecutor is go_executor.refresh_executor: "go" only when enabled and
// the workspace is in the cohort.
func (e ExecutorSettings) RefreshExecutor(workspaceID string) string {
	if e.Enabled && e.Workspaces[workspaceID] {
		return "go"
	}
	return "python"
}

// SignalScanExecutor is go_executor.signal_scan_executor (global, no cohort).
func (e ExecutorSettings) SignalScanExecutor() string {
	if e.Enabled && e.SignalScan {
		return "go"
	}
	return "python"
}

// OutreachExecutor is go_executor.outreach_executor: "go" only when
// GO_EXECUTOR_ENABLED, the workspace is in the cohort and the per-type flag
// (send: GO_EXECUTOR_OUTREACH_SEND, outreach_inbound_poll:
// GO_EXECUTOR_OUTREACH_INBOUND) is on. Other job types are an error, as in Python.
func (e ExecutorSettings) OutreachExecutor(o OutreachSettings, jobType, workspaceID string) (string, error) {
	var flag bool
	switch jobType {
	case "send":
		flag = o.GoExecutorSend
	case "outreach_inbound_poll":
		flag = o.GoExecutorInbound
	default:
		return "", fmt.Errorf("config: not an outreach executor job type: %q", jobType)
	}
	if e.Enabled && flag && e.Workspaces[workspaceID] {
		return "go", nil
	}
	return "python", nil
}
