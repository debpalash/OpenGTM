package scrape

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadscore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// ── holehe_verify.py (the holehe library runs as the browser op) ───────────

type holehe struct{ gated }

func (holehe) Name() string { return "holehe" }

func (holehe) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	email := ls(in.Lead, "email")
	if email == "" || !strings.Contains(email, "@") {
		return fail(in, "No email to verify"), nil
	}
	b := current().Browser
	if b == nil {
		return fail(in, errNoWeb), nil
	}
	res, err := b.Holehe(ctx, browserclient.HoleheRequest{Email: email})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return providers.Output{}, cerr
		}
		// Deviation: Python reported the library exception text; the browser
		// service returns fixed error codes only.
		return fail(in, "holehe unavailable: "+browserCode(err)), nil
	}
	exists := res.ExistsOn
	f := providers.NewObj()
	f.Set("email", email)
	var conf float64
	switch {
	case len(exists) >= 2:
		f.Set("email_verify", "verified_active")
		f.Set("email_presence", strings.Join(exists[:min(5, len(exists))], ", "))
		conf = math.Min(0.95, 0.70+float64(len(exists))*0.05)
	case len(exists) == 1:
		f.Set("email_verify", "likely_valid")
		f.Set("email_presence", strings.Join(exists, ", "))
		conf = 0.65
	default:
		f.Set("email_verify", "no_presence")
		conf = 0.30
	}
	return result(in, f, conf, ""), nil
}

func browserCode(err error) string {
	var be *browserclient.Error
	if errors.As(err, &be) && be.Code != "" {
		return be.Code
	}
	return "unavailable"
}

// ── staffspy_provider.py (the staffspy library runs as the browser op) ─────

var staffSeniority = []string{"intern", "associate", "executive", "specialist", "analyst", "engineer", "manager", "lead",
	"head", "director", "vp", "vice president", "chief", "cxo", "cto", "cfo", "coo", "ceo", "founder", "owner", "partner"}

func seniorityRank(title string) int {
	t := strings.ToLower(title)
	rank := -1
	for i, kw := range staffSeniority {
		if strings.Contains(t, kw) {
			rank = max(rank, i)
		}
	}
	return rank
}

type staffSpy struct{ gated }

func (staffSpy) Name() string { return "staffspy" }

// staffspyUnavailable replaces the legacy message (STAFFSPY_SESSION_FILE and
// pip install no longer apply: the session is the workspace's own secret, the
// library runs in the browser service).
const staffspyUnavailable = "staffspy_unavailable (opt in: STAFFSPY_ENABLED=1 + the workspace's own LINKEDIN_LI_AT_COOKIE secret + BROWSER_STAFFSPY_ENABLED on the browser service; LinkedIn ToS applies)"

// staffspyCodes are the browser service's staffspy result codes; anything else
// (free text that could echo request data) reads as "failed".
var staffspyCodes = map[string]bool{"login_failed": true, "rate_limited": true, "failed": true}

var codeRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

func (staffSpy) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := ls(in.Lead, "company")
	if company == "" {
		return fail(in, "no_company"), nil
	}
	d := current()
	ws := providers.WorkspaceFrom(ctx)
	if !d.StaffSpyEnabled || d.Browser == nil || d.LinkedInCookies == nil || ws == "" {
		return fail(in, staffspyUnavailable), nil
	}
	cookies, err := d.LinkedInCookies(ctx, ws)
	if err != nil {
		return providers.Output{}, errors.New("staffspy: linkedin session lookup failed")
	}
	hasLiAt := false
	var session []string
	for _, c := range cookies {
		if c.Name == "li_at" && c.Value != "" {
			hasLiAt = true
		}
		if len(c.Value) >= 4 {
			session = append(session, c.Value)
		}
	}
	if !hasLiAt {
		return fail(in, staffspyUnavailable), nil
	}
	scrub := func(v string) string {
		for _, sec := range session {
			v = strings.ReplaceAll(v, sec, "[redacted]")
		}
		return v
	}
	const maxResults = 25
	res, err := d.Browser.StaffSpy(ctx, browserclient.StaffSpyRequest{CompanyName: company,
		Location: ls(in.Lead, "city"), MaxResults: maxResults, Cookies: cookies})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return providers.Output{}, cerr
		}
		code := browserCode(err)
		if !codeRe.MatchString(code) {
			code = "unavailable"
		}
		return fail(in, "staffspy failed: "+code), nil
	}
	if res.Error != "" && res.Error != "no_staff_found" {
		code := res.Error
		if !staffspyCodes[code] {
			code = "failed"
		}
		return fail(in, "staffspy failed: "+code), nil
	}
	var roster []browserclient.StaffRow
	for _, r := range res.Staff {
		if strings.TrimSpace(r.Name) != "" {
			roster = append(roster, browserclient.StaffRow{Name: scrub(r.Name), Title: scrub(r.Title),
				LinkedIn: scrub(r.LinkedIn), Location: scrub(r.Location)})
		}
	}
	if len(roster) == 0 {
		return fail(in, "no_staff_found"), nil
	}
	sort.SliceStable(roster, func(i, j int) bool { return seniorityRank(roster[i].Title) > seniorityRank(roster[j].Title) })
	list := []any{}
	for _, r := range roster[:min(maxResults, len(roster))] {
		o := providers.NewObj()
		o.Set("name", r.Name)
		o.Set("title", r.Title)
		o.Set("linkedin", r.LinkedIn)
		o.Set("location", r.Location)
		list = append(list, o)
	}
	return ok(in, 0.8, "decision_makers", pyDumps(list, false), "contact_person", roster[0].Name,
		"contact_title", roster[0].Title), nil
}

// ── company_size_heuristic.py + enrichment/size_heuristic.py ───────────────

var sizeBands = []string{"1-50", "51-200", "201-500", "500+"}

func bandFromCount(n int) string {
	switch {
	case n <= 0:
		return ""
	case n <= 50:
		return "1-50"
	case n <= 200:
		return "51-200"
	case n <= 500:
		return "201-500"
	}
	return "500+"
}

// pyIntOf is int(v) of a lead value (ok=false where Python raises).
func pyIntOf(v any) (int, bool) {
	switch t := v.(type) {
	case nil:
		return 0, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case float64:
		return int(t), !math.IsInf(t, 0) && !math.IsNaN(t)
	case int:
		return t, true
	case int64:
		return int(t), true
	case string:
		if t == "" {
			return 0, true
		}
		n, err := strconv.Atoi(asciiDigits(strings.ReplaceAll(pyStrip(t), "_", "")))
		return n, err == nil
	default:
		if n, ok := v.(interface{ Int64() (int64, error) }); ok {
			if i, err := n.Int64(); err == nil {
				return int(i), true
			}
			if f, ok := v.(interface{ Float64() (float64, error) }); ok {
				if x, err := f.Float64(); err == nil {
					return int(x), true
				}
			}
		}
	}
	return 0, false
}

type vote struct {
	idx, weight int
	label       string
}

var (
	yearRe     = pyRe(`\d{4}`)
	techSplitR = regexp.MustCompile(`[,;|]`)
)

// jsonish decodes a lead JSON field: a string is json.loads'ed, a decoded
// value is used as is.
func jsonish(v any) (any, bool) {
	if s, isStr := v.(string); isStr {
		return decodeJSON(s)
	}
	return v, true
}

func hiringVote(lead map[string]any) *vote {
	raw := lead["hiring_signals"]
	if !truthy(raw) {
		return nil
	}
	d, ok := jsonish(raw)
	if !ok {
		return nil
	}
	data, ok := d.(map[string]any)
	if !ok {
		return nil
	}
	growth := strings.ToLower(pyStrOfLead(data["growth_signal"]))
	total, ok := pyIntOf(orZero(data["total_jobs"]))
	if !ok {
		total = 0
	}
	switch {
	case growth == "hypergrowth" || total >= 10:
		return &vote{2, 3, "jobs"}
	case growth == "rapid_growth" || total >= 5:
		return &vote{1, 3, "jobs"}
	case growth == "growing" || total >= 2:
		return &vote{1, 2, "jobs"}
	case growth == "hiring" || total >= 1:
		return &vote{0, 2, "jobs"}
	}
	return nil
}

// orZero is `x or 0`.
func orZero(v any) any {
	if !truthy(v) {
		return 0
	}
	return v
}

// pyStrOfLead is str(x or "").
func pyStrOfLead(v any) string {
	if !truthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyStrOf(v)
}

func fundingVote(lead map[string]any) *vote {
	// _funding_amount reads funding_amount / last_funding_amount / funding,
	// which are not Lead fields: Lead.from_dict drops them, so it is always 0.
	stage := strings.ToLower(pyStrOfLead(lead["funding_stage"]))
	if stage == "" {
		return nil
	}
	switch {
	case containsAny(stage, []string{"ipo", "public", "series d", "series e", "series f", "series c"}):
		return &vote{3, 3, "funding"}
	case strings.Contains(stage, "series b"):
		return &vote{2, 3, "funding"}
	case strings.Contains(stage, "series a"):
		return &vote{1, 2, "funding"}
	case containsAny(stage, []string{"seed", "angel", "pre-seed", "bootstrap", "grant"}):
		return &vote{0, 1, "funding"}
	}
	return nil
}

func registryVote(lead map[string]any) *vote {
	raw := lead["decision_makers"]
	if !truthy(raw) {
		return nil
	}
	d, ok := jsonish(raw)
	if !ok {
		return nil
	}
	people, ok := d.([]any)
	if !ok || len(people) == 0 {
		return nil
	}
	switch n := len(people); {
	case n >= 8:
		return &vote{2, 2, "registry"}
	case n >= 4:
		return &vote{1, 1, "registry"}
	}
	return &vote{0, 1, "registry"}
}

func footprintVote(lead map[string]any, year int) *vote {
	score := 0
	n := 0
	for _, t := range techSplitR.Split(pyStrOfLead(lead["technologies"]), -1) {
		if pyStrip(t) != "" {
			n++
		}
	}
	if n >= 10 {
		score++
	}
	if runeLen(pyStrOfLead(lead["description"])) >= 600 {
		score++
	}
	if m := yearRe.FindString(pyStrOfLead(lead["founded_year"])); m != "" {
		if y, ok := pyInt(m); ok && year-y >= 20 {
			score++
		}
	}
	if score == 0 {
		return nil
	}
	return &vote{1, 1, "footprint"}
}

// nowYear is the current UTC year (a var for tests).
var nowYear = func() int { return time.Now().UTC().Year() }

func inferCompanySize(lead map[string]any) (band string, conf float64, basis string) {
	if exact, ok := pyIntOf(orZero(lead["employee_count_exact"])); ok && exact > 0 {
		return bandFromCount(exact), 0.95, "exact"
	}
	var votes []vote
	for _, v := range []*vote{hiringVote(lead), fundingVote(lead), registryVote(lead), footprintVote(lead, nowYear())} {
		if v != nil {
			votes = append(votes, *v)
		}
	}
	if len(votes) == 0 {
		return "", 0, ""
	}
	sum, total := 0, 0
	var labels []string
	for _, v := range votes {
		sum += v.idx * v.weight
		total += v.weight
		if !contains(labels, v.label) {
			labels = append(labels, v.label)
		}
	}
	idx := int(math.RoundToEven(float64(sum) / float64(total)))
	idx = max(0, min(len(sizeBands)-1, idx))
	return sizeBands[idx], pyRound3(math.Min(0.6, 0.3+0.1*float64(len(labels)))), "estimated:" + strings.Join(labels, "+")
}

type companySizeHeuristic struct{ gated }

func (companySizeHeuristic) Name() string { return "company_size_heuristic" }

func (companySizeHeuristic) Enrich(_ context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	band, conf, basis := inferCompanySize(in.Lead)
	if band == "" {
		return fail(in, "insufficient_signals"), nil
	}
	return ok(in, conf, "company_size", band, "company_size_basis", basis), nil
}

// ── lead_scorer_provider.py ────────────────────────────────────────────────

type leadScorer struct{ gated }

func (leadScorer) Name() string { return "lead_scorer" }

func (leadScorer) Enrich(_ context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	lead := in.Lead
	if lead == nil {
		lead = map[string]any{}
	}
	score, err := leadscore.ScoreE(leadscore.MapLead(lead), nil)
	if err != nil {
		var pe *leadscore.PyError
		if errors.As(err, &pe) {
			return fail(in, pe.Msg), nil
		}
		return fail(in, err.Error()), nil
	}
	return ok(in, 0.95, "score", score, "score_tier", leadscore.Tier(score)), nil
}

// ResultProvider: the failure result of lead_scorer carried provider "".
func (leadScorer) ResultProvider(out providers.Output) string {
	if out.Error != "" {
		return ""
	}
	return "lead_scorer"
}

