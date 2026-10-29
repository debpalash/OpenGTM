package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// engineTools are the catalog tools the engine executes (plan 15 WP-COPILOT-CORE, WP-COPILOT-TOOLS, WP-CUTOVER).
// The people tools need People (create_people_workbook: ResolvePerson) and enrich_lead needs Providers; without
// them, and for arguments Go cannot answer exactly like Python (a type Python would coerce or raise on in a way
// not mirrored here), the call answers a tool error.
var engineTools = []string{
	"start_collection", "find_similar_leads", "get_enrichment_gaps", "compare_leads",
	"ambitionbox_search", "ambitionbox_jobs", "import_ambitionbox_to_workbook",
	"create_source_workbook", "set_workbook_refresh", "add_agent_column", "add_signal_trigger",
	"track_account_signals",
	"find_people_at_company", "verify_people_at_company", "enrich_people_contacts", "create_people_workbook",
	"suggest_outreach", "draft_grounded_outreach", "draft_plan", "execute_plan",
	"enrich_lead",
}

// EngineToolNames lists the tools the native engine implements.
func EngineToolNames() []string { return append([]string(nil), engineTools...) }

func isEngineTool(name string) bool { return slices.Contains(engineTools, name) }

// keyErr is Python's KeyError text inside execute()'s generic failure result.
func keyErr(name, key string) string { return toolErr(name, "'"+key+"'") }

// engineTool runs one engine tool. handled=false means the call cannot be answered (a tool error); err is an infrastructure
// failure (reported to the model as a generic tool failure by the caller).
func (t *Turn) engineTool(ctx context.Context, c parsedCall) (string, bool, error) {
	if IsDangerous(c.Name) && t.UID != nil {
		var ok bool
		err := t.inTx(ctx, func(tx pgx.Tx) error {
			var err error
			ok, err = identity.HasPermissionTx(ctx, tx, t.WS, *t.UID, "tables.write", "admin", "editor")
			return err
		})
		if err != nil {
			return "", false, err
		}
		if !ok {
			return errInsufficientRole, true, nil
		}
	}
	a := c.Args
	switch c.Name {
	case "find_similar_leads", "get_enrichment_gaps", "compare_leads":
		var res string
		var handled bool
		err := t.inTx(ctx, func(tx pgx.Tx) error {
			var err error
			switch c.Name {
			case "find_similar_leads":
				res, handled, err = findSimilarLeads(ctx, tx, a)
			case "get_enrichment_gaps":
				res, handled, err = enrichmentGaps(ctx, tx, a)
			default:
				res, handled, err = compareLeads(ctx, tx, a)
			}
			return err
		})
		return res, handled, err
	case "set_workbook_refresh":
		return t.setWorkbookRefresh(ctx, a)
	case "add_agent_column":
		return t.addAgentColumn(ctx, a)
	case "add_signal_trigger":
		return t.addSignalTrigger(ctx, a)
	case "start_collection":
		return t.startCollection(ctx, a)
	case "create_source_workbook":
		return t.createSourceWorkbook(ctx, a)
	case "ambitionbox_search":
		return t.ambitionboxSearch(ctx, a)
	case "ambitionbox_jobs":
		return t.ambitionboxJobs(ctx, a)
	case "import_ambitionbox_to_workbook":
		return t.importAmbitionbox(ctx, a)
	case "track_account_signals":
		return t.trackAccountSignals(ctx, a)
	case "find_people_at_company", "verify_people_at_company", "enrich_people_contacts":
		if t.Engine.People == nil {
			return "", false, nil
		}
		switch c.Name {
		case "find_people_at_company":
			return t.findPeople(ctx, a)
		case "verify_people_at_company":
			return t.verifyPeople(ctx, a)
		}
		return t.enrichContacts(ctx, a)
	case "create_people_workbook":
		if t.Engine.ResolvePerson == nil {
			return "", false, nil
		}
		return t.createPeopleWorkbook(ctx, a)
	case "suggest_outreach":
		return t.suggestOutreach(ctx, a)
	case "draft_grounded_outreach":
		return t.draftGroundedOutreach(ctx, a)
	case "draft_plan":
		return t.draftPlan(ctx, a)
	case "execute_plan":
		return t.executePlan(ctx, a)
	case "enrich_lead":
		return t.enrichLead(ctx, a)
	}
	return "", false, nil
}

// ---- lead tools ----

func sval(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func notPlaceholder(s string) bool { return s != "" && s != "N/A" && s != "nan" }

func leadHasEmail(l leadsread.Lead) bool {
	return notPlaceholder(sval(l.Email)) && strings.Contains(*l.Email, "@")
}
func leadHasPhone(l leadsread.Lead) bool    { return notPlaceholder(sval(l.Phone)) }
func leadHasLinkedin(l leadsread.Lead) bool { return notPlaceholder(sval(l.LinkedinURL)) }
func leadHasContact(l leadsread.Lead) bool  { return notPlaceholder(sval(l.ContactPerson)) }

// leadScore is l.score; a NULL score counts as 0 (Python raised TypeError comparing None).
func leadScore(l leadsread.Lead) int64 {
	if l.Score == nil {
		return 0
	}
	return *l.Score
}

// argInt is an integer argument: absent -> dflt; a JSON integer -> itself; anything else -> ok=false (not
// answered: a tool error).
func argInt(a map[string]any, key string, dflt int64) (int64, bool) {
	v, present := a[key]
	if !present {
		return dflt, true
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// argNumber is a numeric argument as float64 plus its str() form.
func argNumber(a map[string]any, key string, dflt int64) (float64, string, bool) {
	v, present := a[key]
	if !present {
		return float64(dflt), strconv.FormatInt(dflt, 10), true
	}
	switch n := v.(type) {
	case int64:
		return float64(n), pyStr(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, "", false
		}
		return n, pyStr(n), true
	}
	return 0, "", false
}

// clampLeadLimit caps model-supplied lead limits (Python is uncapped; see MaxSearchLeadsLimit).
func clampLeadLimit(n int64) int64 { return min(n, MaxSearchLeadsLimit) }

func leadByID(ctx context.Context, tx pgx.Tx, id int64) (*leadsread.Lead, error) {
	ls, err := leadsread.LeadsByIDs(ctx, tx, []int64{id})
	if err != nil || len(ls) == 0 {
		return nil, err
	}
	return &ls[0], nil
}

type similarRow struct {
	ID             int64   `json:"id"`
	Company        string  `json:"company"`
	City           *string `json:"city"`
	Score          *int64  `json:"score"`
	Tier           *string `json:"tier"`
	Specialization *string `json:"specialization"`
	CompanySize    *string `json:"company_size"`
	Email          *string `json:"email"`
	Phone          *string `json:"phone"`
}

func findSimilarLeads(ctx context.Context, tx pgx.Tx, a map[string]any) (string, bool, error) {
	raw, present := a["lead_id"]
	if !present {
		return keyErr("find_similar_leads", "lead_id"), true, nil
	}
	id, ok := asInt64(raw)
	limit, lok := argInt(a, "limit", 10)
	if !ok || !lok || limit < 0 {
		return "", false, nil
	}
	limit = clampLeadLimit(limit)
	lead, err := leadByID(ctx, tx, id)
	if err != nil {
		return "", false, err
	}
	if lead == nil {
		return errLeadNotFound, true, nil
	}
	var similar []leadsread.Lead
	if sval(lead.Specialization) != "" {
		if similar, err = leadsread.ListLeads(ctx, tx, leadsread.ListParams{Search: *lead.Specialization, Limit: int(limit) + 1}); err != nil {
			return "", false, err
		}
	}
	if len(similar) == 0 && sval(lead.City) != "" {
		if similar, err = leadsread.ListLeads(ctx, tx, leadsread.ListParams{City: *lead.City, Limit: int(limit) + 1}); err != nil {
			return "", false, err
		}
	}
	rows := []similarRow{}
	for _, l := range similar {
		if l.ID == lead.ID || int64(len(rows)) >= limit {
			continue
		}
		rows = append(rows, similarRow{l.ID, l.Company, l.City, l.Score, l.ScoreTier, l.Specialization, l.CompanySize, l.Email, l.Phone})
	}
	out, err := pyJSON(struct {
		Reference    string       `json:"reference"`
		SimilarLeads []similarRow `json:"similar_leads"`
		Count        int          `json:"count"`
	}{lead.Company, rows, len(rows)})
	return out, err == nil, err
}

type gapRow struct {
	ID          int64   `json:"id"`
	Company     string  `json:"company"`
	City        *string `json:"city"`
	Score       *int64  `json:"score"`
	Tier        *string `json:"tier"`
	HasEmail    bool    `json:"has_email"`
	HasPhone    bool    `json:"has_phone"`
	HasLinkedin bool    `json:"has_linkedin"`
	Website     *string `json:"website"`
}

func enrichmentGaps(ctx context.Context, tx pgx.Tx, a map[string]any) (string, bool, error) {
	missing, present := a["missing_field"]
	if !present {
		missing = "any"
	}
	minScore, minStr, ok := argNumber(a, "min_score", 30)
	limit, _, lok := argNumber(a, "limit", 15)
	if !ok || !lok {
		return "", false, nil
	}
	all, err := leadsread.ListLeads(ctx, tx, leadsread.ListParams{Limit: 500})
	if err != nil {
		return "", false, err
	}
	gaps := []gapRow{}
	for _, l := range all {
		if float64(leadScore(l)) < minScore {
			continue
		}
		var gap bool
		switch missing {
		case "email":
			gap = !leadHasEmail(l)
		case "phone":
			gap = !leadHasPhone(l)
		case "linkedin":
			gap = !leadHasLinkedin(l)
		case "decision_makers":
			gap = sval(l.DecisionMakers) == ""
		case "any":
			gap = !leadHasEmail(l) || !leadHasPhone(l) || !leadHasLinkedin(l)
		}
		if gap {
			gaps = append(gaps, gapRow{l.ID, l.Company, l.City, l.Score, l.ScoreTier, leadHasEmail(l), leadHasPhone(l), leadHasLinkedin(l), l.Website})
		}
		if float64(len(gaps)) >= limit {
			break
		}
	}
	mb, err := json.Marshal(missing)
	if err != nil {
		return "", false, nil
	}
	out, err := pyJSON(struct {
		Gaps    []gapRow        `json:"gaps"`
		Count   int             `json:"count"`
		Missing json.RawMessage `json:"missing_field"`
		Message string          `json:"message"`
	}{gaps, len(gaps), mb, "Found " + strconv.Itoa(len(gaps)) + " leads (score>=" + minStr + ") missing " + pyStr(missing)})
	return out, err == nil, err
}

type compareRow struct {
	ID               int64   `json:"id"`
	Company          string  `json:"company"`
	City             *string `json:"city"`
	Score            *int64  `json:"score"`
	Tier             *string `json:"tier"`
	Specialization   *string `json:"specialization"`
	CompanySize      *string `json:"company_size"`
	HasEmail         bool    `json:"has_email"`
	HasPhone         bool    `json:"has_phone"`
	HasLinkedin      bool    `json:"has_linkedin"`
	HasContact       bool    `json:"has_contact"`
	FoundedYear      *string `json:"founded_year"`
	RevenueRange     *string `json:"revenue_range"`
	DataCompleteness int64   `json:"data_completeness"`
}

func compareLeads(ctx context.Context, tx pgx.Tx, a map[string]any) (string, bool, error) {
	var ids []any
	if raw, present := a["lead_ids"]; present {
		list, ok := raw.([]any)
		if !ok {
			return "", false, nil
		}
		ids = list
	}
	if len(ids) > 3 {
		ids = ids[:3]
	}
	rows := []compareRow{}
	for _, raw := range ids {
		id, ok := raw.(int64)
		if !ok {
			return "", false, nil
		}
		l, err := leadByID(ctx, tx, id)
		if err != nil {
			return "", false, err
		}
		if l == nil {
			continue
		}
		n := 0
		for _, b := range []bool{leadHasEmail(*l), leadHasPhone(*l), leadHasLinkedin(*l), leadHasContact(*l),
			sval(l.Website) != "", sval(l.Description) != "", sval(l.CompanySize) != ""} {
			if b {
				n++
			}
		}
		rows = append(rows, compareRow{l.ID, l.Company, l.City, l.Score, l.ScoreTier, l.Specialization, l.CompanySize,
			leadHasEmail(*l), leadHasPhone(*l), leadHasLinkedin(*l), leadHasContact(*l), l.FoundedYear, l.RevenueRange,
			int64(math.RoundToEven(float64(n) / 7 * 100))})
	}
	out, err := pyJSON(struct {
		Comparison []compareRow `json:"comparison"`
		Count      int          `json:"count"`
	}{rows, len(rows)})
	return out, err == nil, err
}

// ---- helpers shared by the workbook tools ----

// objJSON renders an ordered object like json.dumps.
func objJSON(o *wbcrud.Obj) (string, error) {
	b, err := o.MarshalJSON()
	if err != nil {
		return "", err
	}
	r, err := ssestream.Reformat(b)
	return string(r), err
}

func errObj(msg string) string { return `{"error": ` + jstr(msg) + `}` }

var errBadStoredJSON = errors.New("copilot: stored workbook JSON is not the expected shape")
