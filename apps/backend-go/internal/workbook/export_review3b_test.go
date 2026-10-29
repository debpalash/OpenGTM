package workbook

import "time"

// SetAgentTimeout shortens the native agent cell deadline; it returns a restore func.
func SetAgentTimeout(d time.Duration) func() {
	prev := agentTimeout
	agentTimeout = d
	return func() { agentTimeout = prev }
}
