package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// governanceRoutesEnabled: GO_GOVERNANCE_ROUTES_ENABLED and PG_LEAD_STORE on. Otherwise nothing
// registers and the reason is logged. GET /api/governance/capabilities stays on FastAPI (D1).
func (d Deps) governanceRoutesEnabled(area string) bool {
	if !d.Flags.Governance.GoGovernanceRoutesEnabled {
		d.logger().Warn("governance routes not registered: GO_GOVERNANCE_ROUTES_ENABLED is false", "area", area)
		return false
	}
	if !d.Flags.PGLeadStore {
		d.logger().Warn("governance routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
		return false
	}
	return true
}

// playbookRoutesEnabled: GO_PLAYBOOK_ROUTES_ENABLED and PG_LEAD_STORE on.
// GET /api/research-playbooks/capabilities stays on FastAPI (D1).
func (d Deps) playbookRoutesEnabled(area string) bool {
	if !d.Flags.Governance.GoPlaybookRoutesEnabled {
		d.logger().Warn("playbook routes not registered: GO_PLAYBOOK_ROUTES_ENABLED is false", "area", area)
		return false
	}
	if !d.Flags.PGLeadStore {
		d.logger().Warn("playbook routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
		return false
	}
	return true
}

// RegisterGovernance registers the 17 /api/governance routes (Tasks 12-15).
func RegisterGovernance(r *httpx.Router, d Deps) {
	if !d.governanceRoutesEnabled("governance") {
		return
	}
	registerGovernanceAudit(r, d)
	registerGovernanceRetention(r, d)
	registerGovernanceRBAC(r, d)
	registerGovernanceSSO(r, d)
}

// RegisterPlaybooks registers the 9 /api/research-playbooks routes (Tasks 16-17).
func RegisterPlaybooks(r *httpx.Router, d Deps) {
	if !d.playbookRoutesEnabled("playbooks") {
		return
	}
	registerPlaybooksRoutes(r, d)
	registerPlaybookRuns(r, d)
}
