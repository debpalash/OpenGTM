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
}

// LoadWbExec reads the WP-WBEXEC switches; a malformed value is a startup error.
func LoadWbExec(get func(string) string) (WbExecSettings, error) {
	var w WbExecSettings
	var err error
	if w.FullRun, err = boolVar(get, "GO_EXECUTOR_FULL_RUN", false); err != nil {
		return w, err
	}
	if w.ProvenanceTracking, err = boolVar(get, "PROVENANCE_TRACKING_ENABLED", false); err != nil {
		return w, err
	}
	return w, nil
}
