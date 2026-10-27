package config

// AIColsSettings are the plan 15 WP-AICOLS switches.
//
// GO_NATIVE_AI_COLUMNS (default false = today's Python worker path) runs the workbook
// ai_formula, research and agent columns, the ai_formula batch pre-pass and the playbook
// research step in the Go worker instead of over the Python worker RPC. Agent columns then
// reserve paid tools through the Go spend ledger, so the agent_paid_tools refusal goes away.
type AIColsSettings struct {
	Native bool // GO_NATIVE_AI_COLUMNS
	// Problems lists malformed values (read as off); log them, do not fail on them.
	Problems []string
}

// LoadAICols reads the WP-AICOLS switches. Like LoadWbExec it never fails: a malformed value
// reads as off and is reported in Problems.
func LoadAICols(get func(string) string) AIColsSettings {
	var a AIColsSettings
	v, err := boolVar(get, "GO_NATIVE_AI_COLUMNS", false)
	if err != nil {
		a.Problems = append(a.Problems, "GO_NATIVE_AI_COLUMNS is not a boolean; reading it as off")
		return a
	}
	a.Native = v
	return a
}
