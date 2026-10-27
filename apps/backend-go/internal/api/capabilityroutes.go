package api

import (
	"net/http"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/certification"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// capabilityClock is the certification clock (nil: the wall clock); tests freeze it.
var capabilityClock func() time.Time

// RegisterCapabilityRoutes registers, behind GO_CAPABILITY_ROUTES_ENABLED (default off, plan 15 WP-ROUTES-A):
//
//   - GET /api/audiences/capabilities, /api/audience-destinations/types, /api/governance/capabilities and
//     /api/research-playbooks/capabilities: the signed-certification maturity catalogs (integrations/certification.py).
//     Auth is the global authenticator plus current_workspace (any member, no role gate), as in the Python routers.
//   - GET /health and GET /api (main.py): public, fixed bodies.
//
// FastAPI's redirect_slashes for every registered route (RegisterTrailingSlash) has its own flag,
// GO_TRAILING_SLASH_COMPAT (default off), so the two cut over independently.
//
// Maturity is fail closed: "supported" only with a valid attested controlled-live certificate; otherwise "beta"
// with certification null. The certificate file is re-read per request, like Python.
func RegisterCapabilityRoutes(r *httpx.Router, d Deps) {
	RegisterTrailingSlashCompat(r, d)
	if d.Pool == nil || d.Identity == nil {
		return
	}
	on, err := config.CapabilityRoutesFrom(d.getenv())
	if err != nil {
		d.Log.Error("GO_CAPABILITY_ROUTES_ENABLED is invalid; capability routes stay on the Python path")
		return
	}
	if !on {
		return
	}
	h := newCapabilityRoutes(d)
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	r.Handle("GET /api/audiences/capabilities", http.HandlerFunc(h.audiences), authn, workspace)
	r.Handle("GET /api/audience-destinations/types", http.HandlerFunc(h.destinationTypes), authn, workspace)
	r.Handle("GET /api/governance/capabilities", http.HandlerFunc(h.governance), authn, workspace)
	r.Handle("GET /api/research-playbooks/capabilities", http.HandlerFunc(h.playbooks), authn, workspace)
	registerHealth(r)
}

type capabilityRoutes struct{ opts func() certification.Options }

func newCapabilityRoutes(d Deps) capabilityRoutes {
	return capabilityRoutes{opts: func() certification.Options {
		return certification.Options{
			Path: d.Flags.CertificationPath, Key: d.Flags.CertificationKey.Reveal(), BuildSHA: d.Flags.BuildSHA,
			Now: capabilityClock,
		}
	}}
}

// audiences is audience_capabilities: the bare catalog list.
func (c capabilityRoutes) audiences(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, certification.AudienceCapabilityCatalog(c.opts()))
}

// destinationTypes is list_destination_types: {"types": integration_catalog()}.
func (c capabilityRoutes) destinationTypes(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"types": certification.IntegrationCatalog(c.opts())})
}

// governance is governance_capabilities: {"capabilities": governance_capability_catalog()}.
func (c capabilityRoutes) governance(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"capabilities": certification.GovernanceCapabilityCatalog(c.opts())})
}

// playbooks is playbook_capabilities: {"capabilities": agent_capability_catalog()}.
func (c capabilityRoutes) playbooks(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"capabilities": certification.AgentCapabilityCatalog(c.opts())})
}
