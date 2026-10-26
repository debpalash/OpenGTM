package api

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// audReadRefreshTimeout bounds POST /{id}/refresh (plan 10 D8); tests shorten it.
var audReadRefreshTimeout = audiencerefresh.RouteTimeout

// registerAudiencesRead registers its routes (plan 10); stub until its task lands.
func registerAudiencesRead(r *httpx.Router, d Deps) {}
