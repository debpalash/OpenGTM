package mcp

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

// leadWriteAllowlist is _LEAD_WRITE_FIELDS: the only lead columns a write tool
// may set. The Lead dataclass types every one of them as a string, so a
// non-string value is refused (D3) instead of raising inside Python.
var leadWriteAllowlist = []string{
	"company", "website", "email", "phone", "contact_person", "contact_title",
	"city", "state", "address", "specialization", "company_size", "description",
	"industry_tags", "linkedin_url", "twitter_url", "facebook_url", "notes",
	"status", "source",
}

// Per-field bounds (D3): generous against real data, far below the 4 MiB frame.
const (
	maxLeadNameLen = 320   // identifiers and short values (company, email, city, ...)
	maxLeadURLLen  = 2048  // website and social URLs
	maxLeadTextLen = 10000 // free text (address, description, notes, tags)
)

var leadTextFields = map[string]bool{"address": true, "description": true, "notes": true, "industry_tags": true}

func leadFieldMax(k string) int {
	switch {
	case leadTextFields[k]:
		return maxLeadTextLen
	case k == "website" || strings.HasSuffix(k, "_url"):
		return maxLeadURLLen
	}
	return maxLeadNameLen
}

// leadWriteFields is _lead_write_fields: allowlisted keys with a non-null value.
// workspace_id and unknown keys are never read. A non-string value returns the
// client-facing message as errMsg.
func leadWriteFields(a Args) (f LeadFields, errMsg string) {
	f = LeadFields{}
	for _, k := range leadWriteAllowlist {
		v, ok := a[k]
		if !ok || v == nil {
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			return nil, k + " must be a string"
		}
		if utf8.RuneCountInString(s) > leadFieldMax(k) {
			return nil, k + " must be at most " + itoa(leadFieldMax(k)) + " characters"
		}
		f[k] = s
	}
	return f, ""
}

// leadWriteID reads lead_id: ("", id) on success, else the client-facing message.
func leadWriteID(a Args) (int64, string) {
	id, ok, err := a.Int("lead_id")
	if err != nil {
		return 0, "lead_id must be an integer"
	}
	if !ok {
		return 0, "lead_id is required"
	}
	return id, ""
}

// leadWriteTools returns create_lead, update_lead and delete_lead. Each runs
// inside the write pipeline's workspace transaction; every id is resolved
// through the scoped LeadPort first, so a foreign id is a miss.
func leadWriteTools(d Deps) []WriteTool {
	return []WriteTool{
		{Name: "create_lead", Capability: CapLeadsWrite, Roles: WriteRoles,
			Apply: func(ctx context.Context, tx pgx.Tx, _ Ctx, a Args) (string, error) {
				return createLead(ctx, tx, d.LeadPort(), a)
			}},
		{Name: "update_lead", Capability: CapLeadsWrite, Roles: WriteRoles,
			Apply: func(ctx context.Context, tx pgx.Tx, _ Ctx, a Args) (string, error) {
				return updateLead(ctx, tx, d.LeadPort(), a)
			}},
		{Name: "delete_lead", Capability: CapLeadsDelete, Roles: AdminRoles,
			Apply: func(ctx context.Context, tx pgx.Tx, _ Ctx, a Args) (string, error) {
				return deleteLead(ctx, tx, d.LeadPort(), a)
			}},
	}
}

func createLead(ctx context.Context, tx pgx.Tx, p LeadPort, a Args) (string, error) {
	f, msg := leadWriteFields(a)
	if msg != "" {
		return errJSON(msg), nil
	}
	company, _ := f["company"].(string)
	if strings.TrimSpace(company) == "" {
		return errJSON("company is required"), nil
	}
	id, err := p.Upsert(ctx, tx, f)
	if err != nil {
		return "", err
	}
	b, err := pyjson.Dumps(pyjson.NewObj("status", "created", "lead_id", id, "company", company), 0)
	return string(b), err
}

func updateLead(ctx context.Context, tx pgx.Tx, p LeadPort, a Args) (string, error) {
	id, msg := leadWriteID(a)
	if msg != "" {
		return errJSON(msg), nil
	}
	f, msg := leadWriteFields(a)
	if msg != "" {
		return errJSON(msg), nil
	}
	lead, err := p.ByID(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if lead == nil {
		return errJSON("Lead not found"), nil
	}
	status, hasStatus := f["status"].(string)
	delete(f, "status")
	names := make([]any, 0, len(f)+1)
	for k := range f {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].(string) < names[j].(string) })
	if len(f) > 0 {
		if err := p.UpdateFields(ctx, tx, id, f); err != nil {
			return "", err
		}
	}
	if hasStatus {
		if err := p.SetStatus(ctx, tx, id, status); err != nil {
			return "", err
		}
		names = append(names, "status")
		sort.Slice(names, func(i, j int) bool { return names[i].(string) < names[j].(string) })
	}
	b, err := pyjson.Dumps(pyjson.NewObj("status", "updated", "lead_id", id, "fields", names), 0)
	return string(b), err
}

func deleteLead(ctx context.Context, tx pgx.Tx, p LeadPort, a Args) (string, error) {
	id, msg := leadWriteID(a)
	if msg != "" {
		return errJSON(msg), nil
	}
	lead, err := p.ByID(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if lead == nil {
		b, err := pyjson.Dumps(pyjson.NewObj("status", "not_found", "lead_id", id), 0)
		return string(b), err
	}
	if err := p.Delete(ctx, tx, id); err != nil {
		return "", err
	}
	b, err := pyjson.Dumps(pyjson.NewObj("status", "deleted", "lead_id", id), 0)
	return string(b), err
}
