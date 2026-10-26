package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// audCrudListCap bounds GET /api/audiences (Python returns every row; documented deviation).
var audCrudListCap = 1000

// registerAudiencesCrud registers its routes (plan 10); stub until its task lands.
func registerAudiencesCrud(r *httpx.Router, d Deps) {}
