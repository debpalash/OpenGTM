package mcp

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	maxWorkbookIDLen    = 64
	defaultNamePrefix   = "MCP — "
	defaultNameRunes    = 40
	msgWorkbookIDLength = "workbook_id must be at most 64 characters"
)

// workbookWriteTools are create_workbook and delete_workbook. Both run inside
// the write pipeline's transaction through WorkbookPort; the workspace is always
// Ctx.WorkspaceID.
func workbookWriteTools(d Deps) []WriteTool {
	return []WriteTool{
		{
			Name: "create_workbook", Capability: CapWorkbooksWrite, Roles: WriteRoles,
			Apply: func(ctx context.Context, tx pgx.Tx, c Ctx, a Args) (string, error) {
				return createWorkbook(ctx, tx, d, c, a)
			},
		},
		{
			Name: "delete_workbook", Capability: CapWorkbooksDelete, Roles: AdminRoles,
			Apply: func(ctx context.Context, tx pgx.Tx, c Ctx, a Args) (string, error) {
				return deleteWorkbook(ctx, tx, d, c, a)
			},
		},
	}
}

func createWorkbook(ctx context.Context, tx pgx.Tx, d Deps, c Ctx, a Args) (string, error) {
	description, ok, err := a.String("description")
	if err != nil {
		return "", err
	}
	if !ok {
		return errorText("description is required"), nil
	}
	name, _, err := a.String("name")
	if err != nil {
		return "", err
	}
	if name == "" { // Python: arguments.get("name") or default
		name = defaultNamePrefix + firstRunes(description, defaultNameRunes)
	}
	cols := planColumns(description)
	id, err := d.WorkbookPort().Create(ctx, tx, c.WorkspaceID, name, description, cols)
	if err != nil {
		return "", err
	}
	names := make([]any, len(cols))
	for i, col := range cols {
		names[i], _ = col.Get("name").(string)
	}
	return dumps(pyjson.NewObj("status", "created", "workbook_id", id, "name", name, "columns", names)), nil
}

func deleteWorkbook(ctx context.Context, tx pgx.Tx, d Deps, c Ctx, a Args) (string, error) {
	id, _, err := a.String("workbook_id")
	if err != nil {
		return "", err
	}
	if id == "" {
		return errorText("workbook_id is required"), nil
	}
	if utf8.RuneCountInString(id) > maxWorkbookIDLen {
		return errorText(msgWorkbookIDLength), nil
	}
	found, err := d.WorkbookPort().Delete(ctx, tx, c.WorkspaceID, id)
	if err != nil {
		return "", err
	}
	status := "not_found"
	if found {
		status = "deleted"
	}
	return dumps(pyjson.NewObj("status", status, "workbook_id", id)), nil
}

func dumps(o *pyjson.Obj) string {
	b, err := pyjson.Dumps(o, 0)
	if err != nil {
		return errorText("internal error")
	}
	return string(b)
}

// firstRunes is Python's s[:n] (code points).
func firstRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// pyLower is str.lower() for the purposes of ASCII keyword matching: Python's
// full case mapping lowers U+0130 to "i" + U+0307 (Go's simple mapping gives "i"),
// which would make "tİtle" contain "title" in Go only.
func pyLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

func hasAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// planColumns ports _plan_workbook_columns: lead_field columns chosen by
// lowercase substring rules; email and contact_person are always present.
func planColumns(description string) []Column {
	lower := pyLower(description)
	cols := []Column{wbcrud.MakeColumn("id", "company", "name", "Company", "type", "lead_field", "lead_field", "company")}
	seen := map[string]bool{"company": true}
	add := func(key, label string) {
		if !seen[key] {
			seen[key] = true
			cols = append(cols, wbcrud.MakeColumn("id", key, "name", label, "type", "lead_field", "lead_field", key))
		}
	}
	if hasAny(lower, "email", "contact") {
		add("email", "Email")
	}
	if hasAny(lower, "phone", "call") {
		add("phone", "Phone")
	}
	if hasAny(lower, "linkedin", "social") {
		add("linkedin_url", "LinkedIn")
	}
	if hasAny(lower, "title", "cto", "ceo", "vp", "founder") {
		add("contact_person", "Contact")
		add("contact_title", "Title")
	}
	if hasAny(lower, "city", "location") {
		add("city", "City")
	}
	add("email", "Email")
	add("contact_person", "Contact")
	return cols
}
