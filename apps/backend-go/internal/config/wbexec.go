package config

// WbExecSettings are the plan 15 WP-WBEXEC switches of the run_workbook executor.
//
// GO_EXECUTOR_FULL_RUN (default false = Python parity) lets runplan route runs to Go that
// Python's go_eligibility refuses (provenance_on, automations_on, condition, email_verify,
// structured_target, column references, formula/http/output columns), and lets the Go
// executor and the single-cell route run them in process instead of handing back or
// proxying to the Python API. PROVENANCE_TRACKING_ENABLED is Python's per-fact provenance
// switch, read here so the Go executor writes the same provenance when it runs such a job.
type WbExecSettings struct {
	FullRun            bool // GO_EXECUTOR_FULL_RUN
	ProvenanceTracking bool // PROVENANCE_TRACKING_ENABLED
	// Problems lists malformed values (each one read as off); log them, do not fail on them.
	Problems []string
}

// LoadWbExec reads the WP-WBEXEC switches for every process that routes or runs a workbook
// (API planner, worker executor, single-cell route). It never fails: a malformed value reads as
// off and is listed in Problems for the caller to log loudly, so all processes agree on what a
// bad value means and a typo in an inactive flag cannot stop a worker (plan 15 review L1/L2).
func LoadWbExec(get func(string) string) WbExecSettings {
	get = AllNative(get) // GO_EXECUTOR_FULL_RUN is retired: always on
	var w WbExecSettings
	read := func(key string) bool {
		v, err := boolVar(get, key, false)
		if err != nil {
			w.Problems = append(w.Problems, key+" is not a boolean; reading it as off")
			return false
		}
		return v
	}
	w.FullRun = read("GO_EXECUTOR_FULL_RUN")
	w.ProvenanceTracking = read("PROVENANCE_TRACKING_ENABLED")
	return w
}
