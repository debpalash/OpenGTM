package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadswrite"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// nativeTools are the catalog tools whose executor is "go" (decision D4). catalog_test pins this list
// against catalog.json so the two cannot drift.
var nativeTools = []string{"search_leads", "get_lead_detail", "get_lead_stats", "update_lead_status"}

// NativeToolNames lists the tools ExecNative implements.
func NativeToolNames() []string { return append([]string(nil), nativeTools...) }

const (
	errInsufficientRole = `{"error": "Insufficient workspace role"}`
	errLeadNotFound     = `{"error": "Lead not found"}`
)

// ExecNative runs one of the four native lead tools inside the caller's tenancy.InWorkspace transaction
// and returns the exact string Python's execute() returns. handled=false means "not mine or arguments I
// cannot answer identically": the caller falls through to the Python worker. A non-nil error is an
// infrastructure failure (the caller reports a generic tool failure; the database error text is not
// echoed into model-visible output here).
//
// uid is nil for self-host (no auth): like Python, the role check is then skipped. For a dangerous tool and
// a real user, tables.write (owner, an explicit per-member allow, or role admin/editor) is required.
func ExecNative(ctx context.Context, tx pgx.Tx, name string, args map[string]any, ws string, uid *int32) (string, bool, error) {
	switch name {
	case "search_leads", "get_lead_detail", "get_lead_stats", "update_lead_status":
	default:
		return "", false, nil
	}
	if IsDangerous(name) && uid != nil {
		ok, err := hasTablesWrite(ctx, tx, ws, *uid)
		if err != nil {
			return "", false, err
		}
		if !ok {
			return errInsufficientRole, true, nil
		}
	}
	switch name {
	case "search_leads":
		return searchLeads(ctx, tx, args)
	case "get_lead_detail":
		return getLeadDetail(ctx, tx, args)
	case "get_lead_stats":
		return getLeadStats(ctx, tx)
	default:
		return updateLeadStatus(ctx, tx, args)
	}
}

// hasTablesWrite is identity.Store.HasPermission(ws, uid, "tables.write", "admin", "editor") on the
// workspace-bound transaction.
func hasTablesWrite(ctx context.Context, tx pgx.Tx, ws string, uid int32) (bool, error) {
	q := gen.New(tx)
	role, err := q.MemberRole(ctx, gen.MemberRoleParams{WorkspaceID: ws, UserID: uid})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if role == "owner" {
		return true, nil
	}
	rows, err := q.MemberPermissions(ctx, gen.MemberPermissionsParams{WorkspaceID: ws, UserID: uid})
	if err != nil {
		return false, err
	}
	effect, found := "", false
	for _, r := range rows {
		if r.Permission == "tables.write" {
			effect, found = r.Effect, true
		}
	}
	if found {
		return effect == "allow", nil
	}
	return role == "admin" || role == "editor", nil
}

// optString is args.get(key) for a string filter: absent or null is "", a string is itself; any other
// type is something Python would hand to SQL as-is, so it is not answered natively.
func optString(args map[string]any, key string) (string, bool) {
	switch v := args[key].(type) {
	case nil:
		return "", true
	case string:
		return v, true
	}
	return "", false
}

// asInt64 accepts the integer spellings a decoded JSON tool-call argument can take.
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == math.Trunc(n) && math.Abs(n) < 1<<53 {
			return int64(n), true
		}
	case json.Number:
		if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(n.String(), 64); err == nil && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return int64(f), true
		}
	}
	return 0, false
}

type searchRow struct {
	ID             int64   `json:"id"`
	Company        string  `json:"company"`
	City           *string `json:"city"`
	Email          *string `json:"email"`
	Phone          *string `json:"phone"`
	Score          *int64  `json:"score"`
	Tier           *string `json:"tier"`
	Status         *string `json:"status"`
	Website        *string `json:"website"`
	Specialization *string `json:"specialization"`
	CompanySize    *string `json:"company_size"`
	ContactPerson  *string `json:"contact_person"`
	Linkedin       *string `json:"linkedin"`
	IndustryTags   *string `json:"industry_tags"`
	FoundedYear    *string `json:"founded_year"`
}

func searchLeads(ctx context.Context, tx pgx.Tx, args map[string]any) (string, bool, error) {
	p := leadsread.ListParams{Limit: 10}
	var ok bool
	if p.Search, ok = optString(args, "query"); !ok {
		return "", false, nil
	}
	if p.City, ok = optString(args, "city"); !ok {
		return "", false, nil
	}
	if p.Status, ok = optString(args, "status"); !ok {
		return "", false, nil
	}
	if p.ScoreTier, ok = optString(args, "score_tier"); !ok {
		return "", false, nil
	}
	if raw, present := args["limit"]; present {
		n, ok := asInt64(raw)
		if !ok || n < 0 {
			return "", false, nil // null/odd/negative limits behave differently in Python's SQL: let it answer
		}
		p.Limit = int(min(n, math.MaxInt32))
	}
	leads, err := leadsread.ListLeads(ctx, tx, p)
	if err != nil {
		return "", false, err
	}
	rows := make([]searchRow, len(leads))
	for i, l := range leads {
		rows[i] = searchRow{
			ID: l.ID, Company: l.Company, City: l.City, Email: l.Email, Phone: l.Phone, Score: l.Score,
			Tier: l.ScoreTier, Status: l.Status, Website: l.Website, Specialization: l.Specialization,
			CompanySize: l.CompanySize, ContactPerson: l.ContactPerson, Linkedin: l.LinkedinURL,
			IndustryTags: l.IndustryTags, FoundedYear: l.FoundedYear,
		}
	}
	out, err := pyJSON(struct {
		Leads []searchRow `json:"leads"`
		Count int         `json:"count"`
	}{rows, len(rows)})
	return out, err == nil, err
}

func getLeadDetail(ctx context.Context, tx pgx.Tx, args map[string]any) (string, bool, error) {
	raw, present := args["lead_id"]
	if !present {
		return `{"error": "Missing required argument: lead_id"}`, true, nil
	}
	id, ok := asInt64(raw)
	if !ok {
		return "", false, nil
	}
	leads, err := leadsread.LeadsByIDs(ctx, tx, []int64{id})
	if err != nil {
		return "", false, err
	}
	if len(leads) == 0 {
		return errLeadNotFound, true, nil
	}
	body, err := leadObject(leads[0])
	if err != nil {
		return "", false, err
	}
	out, err := ssestream.Reformat(append(append([]byte(`{"lead":`), body...), '}'))
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

// leadObject renders Lead.to_dict() in Lead field order with decision_makers parsed from its JSON text
// when it is a non-empty string holding valid JSON (a parse failure keeps the raw string, as in Python).
func leadObject(l leadsread.Lead) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	v, t := reflect.ValueOf(l), reflect.TypeOf(l)
	for i := 0; i < t.NumField(); i++ {
		key := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := canonjson.Marshal(key, false)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		field := v.Field(i)
		if key == "decision_makers" && field.Kind() == reflect.Pointer && !field.IsNil() {
			if s := field.Elem().String(); s != "" {
				if norm, err := ssestream.Reformat([]byte(s)); err == nil {
					b.Write(norm)
					continue
				}
			}
		}
		vb, err := json.Marshal(field.Interface())
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func getLeadStats(ctx context.Context, tx pgx.Tx) (string, bool, error) {
	stats, err := leadsread.Stats(ctx, tx)
	if err != nil {
		return "", false, err
	}
	out, err := pyJSON(stats) // default=str never triggers: every value is already a str/int/float
	return out, err == nil, err
}

func updateLeadStatus(ctx context.Context, tx pgx.Tx, args map[string]any) (string, bool, error) {
	rawID, present := args["lead_id"]
	if !present {
		return `{"error": "Missing required argument: lead_id"}`, true, nil
	}
	rawStatus, present := args["status"]
	if !present {
		return `{"error": "Missing required argument: status"}`, true, nil
	}
	id, ok := asInt64(rawID)
	status, isStr := rawStatus.(string)
	if !ok || !isStr {
		return "", false, nil
	}
	// Python's UPDATE answers ok whether or not a row matched (a foreign id matches none) and ignores note.
	if _, err := leadswrite.UpdateStatus(ctx, tx, id, status); err != nil {
		return "", false, err
	}
	out, err := pyJSON(struct {
		OK        bool   `json:"ok"`
		LeadID    int64  `json:"lead_id"`
		NewStatus string `json:"new_status"`
	}{true, id, status})
	return out, err == nil, err
}

// pyJSON is json.dumps(v) with Python's separators and ensure_ascii, keeping struct field order.
func pyJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	out, err := ssestream.Reformat(b)
	return string(out), err
}
