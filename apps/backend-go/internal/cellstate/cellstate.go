// Package cellstate names what a workbook cell ended as and how a run's status follows from its cells.
//
// The stored cell status stays "complete" / "error" / "skipped" (rows written by every earlier version keep their
// meaning); a cell with status "error" is told apart by its error text, so old and new rows classify the same way:
//
//   - filled: the cell has a value.
//   - no_data: the providers ran and found nothing. A normal result, not a failure.
//   - needs_setup: nothing could run until the user configures something (an LLM key, the browser service, a
//     provider selection, a verification key). The user can act on it.
//   - error: a provider or infrastructure failure (timeout, rate limit, 5xx, crash). Worth a retry.
package cellstate

import "strings"

// Outcome is the class of one cell.
type Outcome string

const (
	Filled     Outcome = "filled"
	NoData     Outcome = "no_data"
	NeedsSetup Outcome = "needs_setup"
	Error      Outcome = "error"
	Skipped    Outcome = "skipped"
)

// Workbook statuses written at the end of a run. "complete" is the existing success value; "completed_with_errors"
// is the value destination runs already use.
const (
	StatusComplete           = "complete"
	StatusCompleteWithErrors = "completed_with_errors"
	StatusFailed             = "failed"
	StatusPaused             = "paused"
)

var noDataTexts = []string{"no_data", "no_answer", "no_email_to_verify", "upstream_dependency_failed", "upstream_dependency_unavailable"}

var needsSetupMarkers = []string{
	"no llm provider is configured", "browser_service_unavailable", "no_providers_selected", "no_default_providers",
	"smtp_port_blocked", "api key is not configured", "missing api key",
}

// OfCell classifies a stored cell by its status and error text.
func OfCell(status, errText string) Outcome {
	switch status {
	case "complete":
		return Filled
	case "skipped":
		return Skipped
	}
	return OfError(errText)
}

// OfError classifies the error text of a cell that has no value.
func OfError(errText string) Outcome {
	e := strings.ToLower(strings.TrimSpace(errText))
	if e == "" {
		return NoData
	}
	for _, m := range needsSetupMarkers {
		if strings.Contains(e, m) {
			return NeedsSetup
		}
	}
	for _, p := range noDataTexts {
		if e == p || strings.HasPrefix(e, p+":") || strings.HasPrefix(e, p+" ") {
			return NoData
		}
	}
	return Error
}

// Counts is the cells of one run by outcome.
type Counts struct {
	Filled, NoData, NeedsSetup, Errors int
}

// RunStatus is the workbook status at the end of a run. A run is "complete" when every cell is filled, found no data
// or needs setup; "completed_with_errors" when some cells ended in an error; "failed" only when the run itself could
// not execute or every cell it attempted ended in an error.
func RunStatus(c Counts, runFailed, stopped bool) string {
	switch {
	case runFailed:
		return StatusFailed
	case stopped:
		return StatusPaused
	case c.Errors == 0:
		return StatusComplete
	case c.Filled+c.NoData+c.NeedsSetup == 0:
		return StatusFailed
	}
	return StatusCompleteWithErrors
}
