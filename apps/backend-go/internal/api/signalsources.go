package api

import (
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/signalscan"
)

// registerSignalSources registers GET /api/signals/sources: the signal-source coverage and
// maturity catalog. Auth as signals.py: the global authenticator plus current_workspace
// (any member, no role gate).
//
// No certification evidence store is wired here, so every source reports maturity "beta"
// with certification null, which is what FastAPI answers when
// OPENGTM_INTEGRATION_CERTIFICATIONS is unset (signalscan.Certifier is the seam).
func registerSignalSources(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	r.Handle("GET /api/signals/sources", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(signalscan.CatalogJSON(nil))
	}), authn, workspace)
}
