// Package govadmin is workspace access administration: the permission catalog,
// the RBAC overview, per-member permission overrides and member management.
// It is the single source of truth for the catalog (Python PERMISSIONS);
// identity.HasPermission takes default roles from its callers' route gates.
package govadmin

import "errors"

// Permission is one catalog entry, in Python PERMISSIONS order.
type Permission struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	Description  string   `json:"description"`
	DefaultRoles []string `json:"default_roles"`
}

// Permissions is the ordered catalog (copied from governance.PERMISSIONS;
// verified against testdata/permissions.json by a test).
var Permissions = []Permission{
	{"tables.write", "Tables", "Create, edit and run workbooks", []string{"editor", "admin"}},
	{"integrations.manage", "Integrations", "Configure enrichment and integration credentials", []string{"editor", "admin"}},
	{"campaigns.write", "Campaigns", "Create and operate campaigns", []string{"editor", "admin"}},
	{"audiences.write", "Audiences", "Create, refresh and schedule audiences", []string{"editor", "admin"}},
	{"activation.write", "Activation", "Configure and run destinations", []string{"editor", "admin"}},
	{"agents.write", "Agents", "Create and run research playbooks", []string{"editor", "admin"}},
	{"signals.write", "Signals", "Scan and manage buying signals", []string{"editor", "admin"}},
	{"outreach.write", "Outreach", "Configure and execute outreach", []string{"admin"}},
	{"automations.manage", "Automations", "Configure trigger automation", []string{"admin"}},
	{"secrets.manage", "Secrets", "Rotate machine credentials", []string{"admin"}},
	{"governance.manage", "Governance", "Audit, retention and access policy", []string{"admin"}},
}

// Typed errors; the HTTP layer maps them to the Python status and detail.
var (
	ErrUnknownPermission = errors.New("Unknown workspace permission")           // 404
	ErrNotMember         = errors.New("Workspace member not found")             // 404
	ErrUserNotFound      = errors.New("User account not found")                 // 404
	ErrOwner             = errors.New("Owner permissions cannot be overridden") // 409
	ErrOwnerRole         = errors.New("Owner role cannot be changed")           // 409
	ErrOwnerRemove       = errors.New("Workspace owner cannot be removed")      // 409
	ErrAlreadyMember     = errors.New("User is already a workspace member")     // 409
	// Validation (422 at the HTTP layer; Python's pydantic enums).
	ErrInvalidRole   = errors.New("role must be viewer, member, editor or admin")
	ErrInvalidEffect = errors.New("effect must be allow, deny or null")
)

func lookup(key string) (Permission, bool) {
	for _, p := range Permissions {
		if p.Key == key {
			return p, true
		}
	}
	return Permission{}, false
}

// AssignableRoles are the roles an admin may grant; owner is never assignable.
var AssignableRoles = []string{"viewer", "member", "editor", "admin"}

func validRole(role string) bool {
	for _, r := range AssignableRoles {
		if r == role {
			return true
		}
	}
	return false
}
