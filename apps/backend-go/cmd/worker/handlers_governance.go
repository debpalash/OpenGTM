package main

// governanceHandlers returns the plan 11 job handlers keyed by job type: retention_enforce
// (GO_EXECUTOR_RETENTION) and research_playbook_run / research_playbook_schedule
// (GO_EXECUTOR_PLAYBOOKS). Scaffold: empty until Task 18 fills it and merges it into the
// dispatch map in main.go. Budget note for Task 18: the Python research_column RPC reports
// cost_usd 0, so the Go playbook run must enforce the budget itself.
func governanceHandlers() map[string]runner {
	return map[string]runner{}
}
