package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

const (
	findLeadsDefaultLimit = 10
	findLeadsMaxLimit     = 200 // D3: Python is unbounded
)

// leadKeys are the Lead.to_dict keys in dataclass order (the json tags of the
// plan 05 Lead, whose Values() uses the same order).
var leadKeys = func() []string {
	t := reflect.TypeOf(Lead{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		keys = append(keys, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
	}
	return keys
}()

// readTools returns find_leads, get_lead_detail and get_pipeline_stats. Every
// handler reads inside Deps.RunInWorkspace bound to the token's workspace.
func readTools(d Deps) []Tool {
	return []Tool{
		{Name: "find_leads", Capability: "leads:read", Handler: func(ctx context.Context, c Ctx, a Args) (string, error) {
			return findLeads(ctx, d, c, a)
		}},
		{Name: "get_lead_detail", Capability: "leads:read", Handler: func(ctx context.Context, c Ctx, a Args) (string, error) {
			return getLeadDetail(ctx, d, c, a)
		}},
		{Name: "get_pipeline_stats", Capability: "leads:read", Handler: func(ctx context.Context, c Ctx, _ Args) (string, error) {
			return getPipelineStats(ctx, d, c)
		}},
	}
}

func errJSON(msg string) string {
	b, _ := pyjson.Dumps(pyjson.NewObj("error", msg), 0)
	return string(b)
}

// ptrStr and ptrInt turn the nullable Lead columns into JSON values.
func ptrStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func findLeads(ctx context.Context, d Deps, c Ctx, a Args) (string, error) {
	query, _, err := a.String("query")
	if err != nil {
		return "", err
	}
	city, _, err := a.String("city")
	if err != nil {
		return "", err
	}
	tier, _, err := a.String("tier")
	if err != nil {
		return "", err
	}
	limit := int64(findLeadsDefaultLimit)
	if n, ok, err := a.Int("limit"); err != nil || (ok && n < 1) {
		return errJSON("limit must be a positive integer"), nil
	} else if ok {
		limit = min(n, findLeadsMaxLimit)
	}
	var leads []Lead
	err = d.RunInWorkspace(ctx, c.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		leads, e = d.LeadPort().List(ctx, tx, ListParams{Search: query, City: city, ScoreTier: tier, Limit: int(limit)})
		return e
	})
	if err != nil {
		return "", err
	}
	rows := make([]any, 0, len(leads))
	for _, l := range leads {
		rows = append(rows, pyjson.NewObj(
			"id", l.ID, "company", l.Company, "city", ptrStr(l.City),
			"email", ptrStr(l.Email), "phone", ptrStr(l.Phone), "score", ptrInt(l.Score),
			"tier", ptrStr(l.ScoreTier), "status", ptrStr(l.Status),
			"website", ptrStr(l.Website), "specialization", ptrStr(l.Specialization),
			"company_size", ptrStr(l.CompanySize), "contact_person", ptrStr(l.ContactPerson),
		))
	}
	return dump(pyjson.NewObj("leads", rows, "count", int64(len(rows))))
}

func getLeadDetail(ctx context.Context, d Deps, c Ctx, a Args) (string, error) {
	id, ok, err := a.Int("lead_id")
	if err != nil {
		return errJSON("lead_id must be an integer"), nil
	}
	if !ok {
		return errJSON("lead_id is required"), nil
	}
	var lead *Lead
	err = d.RunInWorkspace(ctx, c.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		lead, e = d.LeadPort().ByID(ctx, tx, id)
		return e
	})
	if err != nil {
		return "", err
	}
	if lead == nil {
		return errJSON("Lead not found"), nil
	}
	vals := lead.Values()
	o := pyjson.NewObj()
	for i, k := range leadKeys {
		o.Set(k, vals[i])
	}
	if s, _ := o.Vals["decision_makers"].(string); s != "" && json.Valid([]byte(s)) {
		// Python parses a truthy JSON string; anything else stays a string.
		o.Set("decision_makers", json.RawMessage(s))
	}
	out, err := dump(o)
	if err != nil && o.Vals["decision_makers"] != nil {
		o.Set("decision_makers", vals[leadIndexOf("decision_makers")])
		return dump(o)
	}
	return out, err
}

func leadIndexOf(key string) int {
	for i, k := range leadKeys {
		if k == key {
			return i
		}
	}
	return -1
}

func getPipelineStats(ctx context.Context, d Deps, c Ctx) (string, error) {
	var st StatsResult
	err := d.RunInWorkspace(ctx, c.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		st, e = d.LeadPort().Stats(ctx, tx)
		return e
	})
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return dump(json.RawMessage(raw))
}

func dump(v any) (string, error) {
	b, err := pyjson.Dumps(v, 2)
	return string(b), err
}
