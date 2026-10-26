package api

import (
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// autoRunNow is the clock of the manual-run fire key (tests pin it).
var autoRunNow = func() time.Time { return time.Now().UTC() }

// registerAutomationsRun registers the automations preview/run/runs (Task 18) routes; a stub until that task lands.
func registerAutomationsRun(r *httpx.Router, d Deps) {}
