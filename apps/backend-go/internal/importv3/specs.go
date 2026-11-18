// Package importv3 copies the data a v3.0.0 (Python) installation kept in SQLite files under data/ into the
// PostgreSQL tables the Go runtime reads: workspaces and their members/settings/SCIM/OIDC rows, the global
// settings (provider keys), chat history, workbook functions and chat memories.
//
// It is the Go port of legacy/python apps/api/scripts/sqlite_stores (specs.py, backfill.py), so `opengtm-migrate
// import-v3` needs no Python. It is only linked into cmd/migrate: the pure-Go SQLite reader stays out of the api,
// worker, scheduler and mcp binaries.
//
// Rules: SQLite files are read from a private copy and never modified; ciphertext (enc:v1: ...) is copied verbatim,
// never decrypted (the same SECRET_KEY is therefore required to read it); a run inserts rows that are missing and
// never overwrites a row that already exists in PostgreSQL, so it is idempotent and a re-run cannot revert changes
// made in the Go app; each store (SQLite file) is one transaction.
package importv3

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Row is one source row keyed by PostgreSQL column name.
type Row map[string]any

// Spec maps one SQLite table to one PostgreSQL table.
type Spec struct {
	Name     string // PostgreSQL table
	File     string // SQLite file under the data dir
	Table    string // SQLite table
	Columns  []string
	PK       []string
	Tenant   string              // column holding workspace_id for FORCE-RLS tables
	Derived  []string            // columns without a SQLite source (filled by PostLoad)
	JSON     map[string]string   // column -> default JSON text for jsonb columns
	Exclude  map[string][]string // column -> values never copied (the Python cutover marker)
	Convert  map[string]func(any) (any, error)
	UserCol  string // users.id reference, for orphan reporting only
	FKs      []FK   // foreign keys PostgreSQL enforces and SQLite never did
	OrderBy  string // SQLite ORDER BY
	Sequence string // serial column whose sequence is advanced past max(id)
	PostLoad func(rows []Row, loaded map[string][]Row) (kept []Row, skipped int)
}

// FK is a foreign key checked before the first write.
type FK struct {
	Cols   []string
	Parent string
	PCols  []string
}

const storesBackendMarker = "__stores_backend"

func nullTo(def any) func(any) (any, error) {
	return func(v any) (any, error) {
		if v == nil {
			return def, nil
		}
		return v, nil
	}
}

func intToBool(v any) (any, error) {
	switch t := v.(type) {
	case nil:
		return false, nil
	case int64:
		return t != 0, nil
	case float64:
		return t != 0, nil
	case bool:
		return t, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n != 0, err
	}
	return nil, fmt.Errorf("unsupported boolean value %T", v)
}

var tsLayouts = []string{
	"2006-01-02 15:04:05", "2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05", "2006-01-02T15:04:05.999999999",
	time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05Z07:00",
}

// parseTS accepts SQLite's datetime('now') ("YYYY-MM-DD HH:MM:SS", UTC) and ISO-8601 strings with or without an
// offset; the result is UTC.
func parseTS(v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("timestamp is %T, not text", v)
	}
	s = strings.TrimSpace(s)
	for _, l := range tsLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return nil, fmt.Errorf("unparseable timestamp %q", s)
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// Conversations without a workspace or user are invisible today (every query rejects falsy ids); importing them
// would expose private chats, so they are skipped and counted.
func skipUntenantedConversations(rows []Row, _ map[string][]Row) ([]Row, int) {
	kept := rows[:0:0]
	for _, r := range rows {
		if str(r["workspace_id"]) != "" && str(r["user_id"]) != "" {
			kept = append(kept, r)
		}
	}
	return kept, len(rows) - len(kept)
}

// Messages take their workspace from the parent conversation; a message whose parent was skipped is skipped.
// SQLite timestamps have one-second resolution and ties were ordered by rowid, while PostgreSQL orders by
// created_at only, so same-instant messages of one conversation get ordinal microsecond offsets (rows arrive in
// rowid order).
func messagesFromParents(rows []Row, loaded map[string][]Row) ([]Row, int) {
	ws := map[string]string{}
	for _, c := range loaded["chat_conversations"] {
		ws[str(c["id"])] = str(c["workspace_id"])
	}
	type key struct {
		conv string
		at   time.Time
	}
	ties := map[key]int{}
	kept := rows[:0:0]
	for _, r := range rows {
		w, ok := ws[str(r["conversation_id"])]
		if !ok {
			continue
		}
		r["workspace_id"] = w
		at := r["created_at"].(time.Time)
		k := key{str(r["conversation_id"]), at}
		if n := ties[k]; n > 0 {
			r["created_at"] = at.Add(time.Duration(n) * time.Microsecond)
		}
		ties[k]++
		kept = append(kept, r)
	}
	return kept, len(rows) - len(kept)
}

const wsFile = "workspaces.db"

// Specs lists every imported table, parents before children.
func Specs() []Spec {
	empty := nullTo("")
	return []Spec{
		{Name: "workspaces", File: wsFile, Table: "workspaces",
			Columns: []string{"id", "name", "slug", "description", "icon", "owner_id", "created_at", "updated_at"},
			PK:      []string{"id"}, UserCol: "owner_id",
			Convert: map[string]func(any) (any, error){"description": empty, "icon": nullTo("\U0001F3E2")}},
		{Name: "workspace_members", File: wsFile, Table: "workspace_members",
			Columns: []string{"workspace_id", "user_id", "role", "created_at"},
			PK:      []string{"workspace_id", "user_id"}, UserCol: "user_id",
			FKs:     []FK{{[]string{"workspace_id"}, "workspaces", []string{"id"}}},
			Convert: map[string]func(any) (any, error){"role": nullTo("member")}},
		{Name: "user_active_workspace", File: wsFile, Table: "user_active_workspace",
			Columns: []string{"user_id", "workspace_id"}, PK: []string{"user_id"}, UserCol: "user_id"},
		{Name: "workspace_oidc_identities", File: wsFile, Table: "workspace_oidc_identities",
			Columns: []string{"workspace_id", "issuer", "subject", "user_id", "email", "created_at"},
			PK:      []string{"workspace_id", "issuer", "subject"}, UserCol: "user_id",
			FKs:     []FK{{[]string{"workspace_id", "user_id"}, "workspace_members", []string{"workspace_id", "user_id"}}},
			Convert: map[string]func(any) (any, error){"email": empty}},
		{Name: "workspace_scim_tokens", File: wsFile, Table: "workspace_scim_tokens",
			Columns: []string{"workspace_id", "token_hash", "token_prefix", "created_at", "expires_at", "last_used_at", "created_by"},
			PK:      []string{"workspace_id"}, UserCol: "created_by",
			FKs: []FK{{[]string{"workspace_id"}, "workspaces", []string{"id"}}}},
		{Name: "workspace_settings", File: wsFile, Table: "workspace_settings",
			Columns: []string{"workspace_id", "key", "value"}, PK: []string{"workspace_id", "key"}, Tenant: "workspace_id",
			Convert: map[string]func(any) (any, error){"value": empty},
			FKs:     []FK{{[]string{"workspace_id"}, "workspaces", []string{"id"}}}},
		{Name: "workspace_member_permissions", File: wsFile, Table: "workspace_member_permissions",
			Columns: []string{"workspace_id", "user_id", "permission", "effect", "updated_at"},
			PK:      []string{"workspace_id", "user_id", "permission"}, Tenant: "workspace_id", UserCol: "user_id",
			FKs: []FK{{[]string{"workspace_id", "user_id"}, "workspace_members", []string{"workspace_id", "user_id"}}}},
		{Name: "workspace_scim_users", File: wsFile, Table: "workspace_scim_users",
			Columns: []string{"workspace_id", "user_id", "external_id", "display_name", "active", "created_at", "updated_at"},
			PK:      []string{"workspace_id", "user_id"}, Tenant: "workspace_id", UserCol: "user_id",
			FKs:     []FK{{[]string{"workspace_id"}, "workspaces", []string{"id"}}},
			Convert: map[string]func(any) (any, error){"active": intToBool, "external_id": empty, "display_name": empty}},
		{Name: "workspace_scim_groups", File: wsFile, Table: "workspace_scim_groups",
			Columns: []string{"id", "workspace_id", "external_id", "display_name", "created_at", "updated_at"},
			PK:      []string{"id"}, Tenant: "workspace_id",
			FKs:     []FK{{[]string{"workspace_id"}, "workspaces", []string{"id"}}},
			Convert: map[string]func(any) (any, error){"external_id": empty}},
		{Name: "workspace_scim_group_members", File: wsFile, Table: "workspace_scim_group_members",
			Columns: []string{"workspace_id", "group_id", "user_id", "created_at"},
			PK:      []string{"workspace_id", "group_id", "user_id"}, Tenant: "workspace_id", UserCol: "user_id",
			FKs: []FK{{[]string{"group_id"}, "workspace_scim_groups", []string{"id"}}}},
		{Name: "app_settings", File: "data.db", Table: "settings", Columns: []string{"key", "value"}, PK: []string{"key"},
			Convert: map[string]func(any) (any, error){"value": empty},
			Exclude: map[string][]string{"key": {storesBackendMarker}}},

		{Name: "chat_conversations", File: "chat_history.db", Table: "conversations",
			Columns: []string{"id", "title", "workspace_id", "user_id", "created_at", "updated_at"},
			PK:      []string{"id"}, Tenant: "workspace_id", PostLoad: skipUntenantedConversations,
			Convert: map[string]func(any) (any, error){"created_at": parseTS, "updated_at": parseTS}},
		{Name: "chat_messages", File: "chat_history.db", Table: "messages",
			Columns: []string{"id", "conversation_id", "workspace_id", "role", "content", "tool_data", "created_at"},
			PK:      []string{"id"}, Tenant: "workspace_id", Derived: []string{"workspace_id"},
			PostLoad: messagesFromParents, OrderBy: "rowid",
			Convert: map[string]func(any) (any, error){"created_at": parseTS}},
		{Name: "chat_tool_approvals", File: "chat_history.db", Table: "tool_approvals",
			Columns: []string{"id", "workspace_id", "user_id", "tool_call_json", "status", "created_at_epoch"},
			PK:      []string{"id"}, Tenant: "workspace_id"},
		{Name: "workbook_functions", File: "functions.db", Table: "functions",
			Columns: []string{"id", "name", "description", "category", "columns_chain", "version", "usage_count", "created_at", "updated_at"},
			PK:      []string{"id"}, JSON: map[string]string{"columns_chain": "[]"}},
		{Name: "chat_memories", File: "chat_memory.db", Table: "memories",
			Columns: []string{"id", "user_id", "text", "metadata", "created_at"},
			PK:      []string{"id"}, JSON: map[string]string{"metadata": "{}"}, Sequence: "id",
			Convert: map[string]func(any) (any, error){"created_at": parseTS}},
	}
}
