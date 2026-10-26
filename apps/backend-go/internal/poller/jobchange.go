package poller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// Person-level job-change detection (job_change.py). Detection is a
// conservative DDG lookup through the Python worker (`ddgs_search`); the
// current employer is parsed only from high-confidence title/snippet patterns.
// Every Python regex is compiled once here; where RE2 differs from Python's
// Unicode-aware `re` the pattern is rewritten (explicit classes, manual \b) and
// pinned by vectors, never approximated.

// JobChangeWeight is JOB_CHANGE_WEIGHT.
const JobChangeWeight = 9

// JobChangeStateKey is STATE_KEY: the cursor sub-key holding per-contact state.
const JobChangeStateKey = "job_change_contacts"

const jobChangeSource = "ddg_linkedin"

var (
	// _SUFFIX_RE runs on text already reduced to [a-z0-9 ], so RE2's ASCII \b is
	// exactly Python's.
	jcSuffixRE = regexp.MustCompile(`\b(inc|incorporated|corp|corporation|llc|l\.l\.c|llp|lp|ltd|limited|` +
		`co|company|plc|pvt|private|pte|pty|gmbh|ag|sa|sarl|srl|bv|nv|oy|ab|kk|` +
		`holdings|group|the)\b`)
	jcNonAlnumRE = regexp.MustCompile(`[^a-z0-9 ]+`)
	jcSpaceRE    = regexp.MustCompile(`[` + pcSpace + `]+`)

	// linkedin\.com/in/([A-Za-z0-9_.%\-]+) with IGNORECASE: under Python's
	// Unicode folding the literal i matches U+0130/U+0131, the literal k
	// U+212A, and [A-Za-z] also U+0130 U+0131 U+017F U+212A.
	jcHandleRE = regexp.MustCompile(`(?i)l` + pcI + `n[kK\x{212A}]ed` + pcI + `n\.com/` + pcI +
		`n/([A-Za-z0-9_.%\-\x{130}\x{131}\x{17F}\x{212A}]+)`)

	jcLITailRE     = regexp.MustCompile(`(?i)[` + pcSpace + `]*[|\-–—][` + pcSpace + `]*l` + pcI + `n[kK\x{212A}]ed` + pcI + `n[` + pcSpace + `]*$`)
	jcTitleSplitRE = regexp.MustCompile(`[` + pcSpace + `]+[-–—|][` + pcSpace + `]+`)
	jcAtCompanyRE  = regexp.MustCompile(`(?i)^(?:.+?)[` + pcSpace + `]+(?:at|@)[` + pcSpace + `]+(.+)$`)
	// \bexperience:\s*([^·|•]+): the \b is checked by hand (Unicode word chars),
	// so the pattern is split into the literal and the tail.
	jcExperienceLitRE  = regexp.MustCompile(`(?i)exper` + pcI + `ence:`)
	jcExperienceTailRE = regexp.MustCompile(`^[` + pcSpace + `]*([^·|•]+)`)
)

var jcStopwords = map[string]bool{
	"linkedin": true, "self employed": true, "self-employed": true, "freelance": true,
	"freelancer": true, "open to work": true, "unemployed": true, "student": true, "retired": true,
}

// NormalizeCompany is normalize_company: lowercase, strip punctuation and legal
// suffixes, collapse whitespace.
func NormalizeCompany(name string) string {
	s := pcLower(name)
	s = jcNonAlnumRE.ReplaceAllString(s, " ")
	s = jcSuffixRE.ReplaceAllString(s, " ")
	return providers.PyStrip(jcSpaceRE.ReplaceAllString(s, " "))
}

func normPerson(name string) string {
	s := pcLower(name)
	s = jcNonAlnumRE.ReplaceAllString(s, " ")
	return providers.PyStrip(jcSpaceRE.ReplaceAllString(s, " "))
}

// LinkedInHandle is linkedin_handle: the lowercased /in/<handle> of a profile URL.
func LinkedInHandle(url string) string {
	m := jcHandleRE.FindStringSubmatch(providers.PyStrip(url))
	if m == nil {
		return ""
	}
	return strings.TrimRight(pcLower(m[1]), "/")
}

// Contact is one tracked person. LeadID is nil for explicit roster entries.
type Contact struct {
	LeadID      *int64
	Name        string
	Company     string
	LinkedInURL string
	Key         string
}

// ContactKey is contact_key: lead id, then linkedin handle, then name|company.
func ContactKey(c Contact) string {
	if c.LeadID != nil && *c.LeadID != 0 {
		return "lead:" + strconv.FormatInt(*c.LeadID, 10)
	}
	if h := LinkedInHandle(c.LinkedInURL); h != "" {
		return "li:" + h
	}
	return "nm:" + normPerson(c.Name) + "|" + NormalizeCompany(c.Company)
}

// BuildQuery is build_query: `"<name>" <linkedin-handle-or-company> site:linkedin.com/in`.
// Only the tracked person's name, handle and company ever enter the query.
func BuildQuery(name, linkedinURL, company string) string {
	anchor := LinkedInHandle(linkedinURL)
	if anchor == "" {
		anchor = providers.PyStrip(company)
	}
	parts := []string{`"` + providers.PyStrip(name) + `"`}
	if anchor != "" {
		parts = append(parts, anchor)
	}
	parts = append(parts, "site:linkedin.com/in")
	return strings.Join(parts, " ")
}

func plausibleCompany(candidate, person string) bool {
	c := providers.PyStrip(candidate)
	if n := utf8.RuneCountInString(c); n < 2 || n > 80 {
		return false
	}
	norm := NormalizeCompany(c)
	if norm == "" {
		return false
	}
	if jcStopwords[pcLower(c)] || jcStopwords[norm] {
		return false
	}
	if strings.Contains(norm, "linkedin") {
		return false
	}
	if norm == normPerson(person) {
		return false
	}
	return true
}

// firstExperience is _EXPERIENCE_RE.search(snippet).group(1) with Python's
// Unicode \b before "experience:".
func firstExperience(snippet string) (string, bool) {
	for _, loc := range jcExperienceLitRE.FindAllStringIndex(snippet, -1) {
		start, end := loc[0], loc[1]
		if start > 0 {
			r, _ := utf8.DecodeLastRuneInString(snippet[:start])
			if isWordRune(r) {
				continue
			}
		}
		if m := jcExperienceTailRE.FindStringSubmatch(snippet[end:]); m != nil {
			return m[1], true
		}
	}
	// Overlapping literal occurrences are impossible ("experience:" cannot
	// overlap itself), so the non-overlapping scan above is exhaustive.
	return "", false
}

// CompanyFromResult is _company_from_result: (company, ok, nameMatched) from one
// linkedin.com/in result row. ok is false when no confident employer was parsed.
func CompanyFromResult(title, snippet, name string) (company string, ok, nameMatched bool) {
	cleaned := jcLITailRE.ReplaceAllString(providers.PyStrip(title), "")
	var segments []string
	for _, s := range jcTitleSplitRE.Split(cleaned, -1) {
		if s = providers.PyStrip(s); s != "" {
			segments = append(segments, s)
		}
	}
	if len(segments) > 0 {
		first, _, _ := strings.Cut(segments[0], ",")
		nameMatched = name != "" && normPerson(first) == normPerson(name)
	}
	candidate, have := "", false
	switch {
	case len(segments) >= 3:
		candidate, have = segments[len(segments)-1], true
	case len(segments) == 2:
		if m := jcAtCompanyRE.FindStringSubmatch(segments[1]); m != nil {
			candidate, have = m[1], true
		}
	}
	if !have && snippet != "" {
		if c, found := firstExperience(snippet); found {
			candidate, have = c, true
		}
	}
	if !have {
		return "", false, nameMatched
	}
	candidate = providers.PyStrip(strings.Trim(providers.PyStrip(candidate), "·•|"))
	if !plausibleCompany(candidate, name) {
		return "", false, nameMatched
	}
	return candidate, true, nameMatched
}

// SearchResult is one search row. URL is `href or url`; Snippet is `body or snippet`.
type SearchResult struct {
	URL, Title, Snippet string
}

// Searcher runs one DDG query. A transport failure is an error.
type Searcher func(ctx context.Context, query string) ([]SearchResult, error)

// ResolveCurrentCompany is resolve_current_company: the person's current
// employer parsed conservatively, with the evidence URL. Identity gate: a known
// linkedin handle must match the result URL's; otherwise the result-title name
// must equal the tracked name (normalized). Search errors propagate.
func ResolveCurrentCompany(ctx context.Context, name, linkedinURL, companyHint string, search Searcher) (company, evidence string, found bool, err error) {
	if providers.PyStrip(name) == "" && LinkedInHandle(linkedinURL) == "" {
		return "", "", false, nil
	}
	results, err := search(ctx, BuildQuery(name, linkedinURL, companyHint))
	if err != nil {
		return "", "", false, err
	}
	want := LinkedInHandle(linkedinURL)
	for _, r := range results {
		got := LinkedInHandle(r.URL)
		if got == "" {
			continue
		}
		if want != "" && got != want {
			continue
		}
		cand, ok, matched := CompanyFromResult(r.Title, r.Snippet, name)
		if !ok {
			continue
		}
		if !(want != "" && got == want) && !matched {
			continue
		}
		u, _, _ := strings.Cut(r.URL, "?")
		return cand, strings.TrimRight(u, "/"), true, nil
	}
	return "", "", false, nil
}

// pcInt is Python int(x) for the shapes a JSON config value takes; ok is false
// where Python raises (the caller treats that as "no usable number").
func pcInt(v any) (int64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) || t >= 9.2e18 || t <= -9.2e18 {
			return 0, false
		}
		return int64(t), true
	case string:
		n, ok := parsePyInt(t)
		if !ok || !n.IsInt64() {
			return 0, false
		}
		return n.Int64(), true
	}
	return 0, false
}

// PerPollCap is per_poll_cap: the global cap, lowerable (never raisable) by the
// watch's max_contacts_per_poll; never below 1.
func PerPollCap(globalCap int, config map[string]any) int {
	cap := int64(globalCap)
	if ov := config["max_contacts_per_poll"]; providers.Truthy(ov) {
		if n, ok := pcInt(ov); ok {
			cap = max(1, min(cap, n))
		}
	}
	return int(max(1, cap))
}

// MaterializeContacts is materialize_contacts: it expands config.contacts into
// concrete contacts. lead_id entries resolve from the workspace's lead rows
// (call inside tenancy.InWorkspace; a foreign or unknown lead is skipped);
// entries without a usable person name or linkedin handle are skipped.
// A lead_id that is not an integer is skipped (Python would raise).
func MaterializeContacts(ctx context.Context, tx pgx.Tx, w PyWatch) ([]Contact, error) {
	entries, _ := w.Config["contacts"].([]any)
	out := []Contact{}
	q := gen.New(tx)
	for _, e := range entries {
		entry := asMap(e)
		if entry == nil {
			continue
		}
		var c Contact
		if lv := entry["lead_id"]; providers.Truthy(lv) {
			id, ok := pcInt(lv)
			if !ok || id < math.MinInt32 || id > math.MaxInt32 {
				continue
			}
			row, err := q.PollContactLead(ctx, gen.PollContactLeadParams{ID: int32(id), WorkspaceID: w.WorkspaceID})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("lead lookup: %w", err)
			}
			name := pcStrField(entry, "name")
			if name == "" {
				name = textOr(row.ContactPerson)
			}
			li := pcStrField(entry, "linkedin_url")
			if LinkedInHandle(li) == "" {
				li = ""
				if rl := textOr(row.LinkedinUrl); LinkedInHandle(rl) != "" {
					li = rl
				}
			}
			company := pcStrField(entry, "company")
			if company == "" {
				company = row.Company
			}
			lid := id
			c = Contact{LeadID: &lid, Name: providers.PyStrip(name), Company: providers.PyStrip(company), LinkedInURL: li}
		} else {
			c = Contact{
				Name:        providers.PyStrip(pcStrField(entry, "name")),
				Company:     providers.PyStrip(pcStrField(entry, "company")),
				LinkedInURL: providers.PyStrip(pcStrField(entry, "linkedin_url")),
			}
		}
		if c.Name == "" && LinkedInHandle(c.LinkedInURL) == "" {
			continue
		}
		c.Key = ContactKey(c)
		out = append(out, c)
	}
	return out, nil
}

// pcStrField is `entry.get(key) or ""` for string values; a non-string value is
// treated as absent (Python would raise on .strip()).
func pcStrField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func textOr(t pgtype.Text) string {
	if t.Valid {
		return t.String
	}
	return ""
}

// FetchJobChanges is fetch_job_changes over the Python `ddgs_search` RPC: it
// checks up to the per-poll cap of tracked contacts (least recently checked
// first) and emits job_change events on confident employer changes. The state
// lives in cursor key job_change_contacts. backfill is ignored: a first
// observation can never prove a change.
//
// When EVERY attempted lookup failed the result is (nil, nil, err) and the
// cursor is left alone. A pyworker timeout, outage or auth failure stops the
// batch (the rest of the roster is retried next poll, least recently checked
// first), so a hung worker costs at most one SearchTimeout.
func FetchJobChanges(ctx context.Context, d PyDeps, w PyWatch, contacts []Contact, backfill bool) ([]DetectedEvent, map[string]any, error) {
	n := 0
	search := func(ctx context.Context, query string) ([]SearchResult, error) {
		idx := n
		n++
		fields, err := d.Py.pcCall(ctx, w, "job_change", idx, ddgsProvider,
			map[string]any{"query": query, "max_results": ddgSearchMaxResultsInput}, d.Py.searchTimeout())
		if err != nil {
			return nil, err
		}
		rows, ok := fields["results"].([]any)
		if !ok {
			return nil, &PyCallError{Msg: "search results missing"}
		}
		res := make([]SearchResult, 0, len(rows))
		for _, r := range rows {
			m := asMap(r)
			if m == nil {
				continue
			}
			res = append(res, SearchResult{URL: pcStrField(m, "url"), Title: pcStrField(m, "title"), Snippet: pcStrField(m, "snippet")})
		}
		return res, nil
	}
	return fetchJobChangesWith(ctx, w.Cursor, w.Config, d.Py.jobChangeCap(), contacts, d.now(), search)
}

func pcFloat(v any) float64 {
	if f, ok := providers.Num(v); ok {
		return f
	}
	if s, ok := v.(string); ok {
		if f, err := strconv.ParseFloat(providers.PyStrip(s), 64); err == nil {
			return f
		}
	}
	return 0
}

func fetchJobChangesWith(ctx context.Context, cursor, config map[string]any, globalCap int, contacts []Contact, now time.Time, search Searcher) ([]DetectedEvent, map[string]any, error) {
	ts := float64(now.UnixNano()) / 1e9
	state := copyMap(asMap(cursor[JobChangeStateKey]))

	ordered := append([]Contact(nil), contacts...)
	last := func(c Contact) float64 { return pcFloat(asMap(state[c.Key])["last_checked_at"]) }
	sort.SliceStable(ordered, func(i, j int) bool { return last(ordered[i]) < last(ordered[j]) })
	if c := PerPollCap(globalCap, config); len(ordered) > c {
		ordered = ordered[:c]
	}

	events := []DetectedEvent{}
	attempted, errored := 0, 0
	var lastErr error
	for _, contact := range ordered {
		key := contact.Key
		attempted++
		resolved, evidence, found, err := ResolveCurrentCompany(ctx, contact.Name, contact.LinkedInURL, contact.Company, search)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			errored++
			lastErr = err
			if isBatchStop(err) {
				break
			}
			continue // no state bump: stays first in LRU order next cycle
		}

		st := copyMap(asMap(state[key]))
		prev := ""
		if s, ok := st["last_known_company"].(string); ok {
			prev = providers.PyStrip(s)
		} else if providers.Truthy(st["last_known_company"]) {
			prev = providers.PyStrip(providers.Str(st["last_known_company"]))
		}

		if !providers.Truthy(state[key]) {
			base := resolved
			if !found || base == "" {
				base = contact.Company
			}
			state[key] = map[string]any{"last_known_company": base, "last_checked_at": ts}
			continue
		}

		st["last_checked_at"] = ts
		if !found {
			state[key] = st
			continue
		}
		if prev == "" {
			st["last_known_company"] = resolved
			state[key] = st
			continue
		}
		oldN, newN := NormalizeCompany(prev), NormalizeCompany(resolved)
		if oldN != "" && newN != "" && oldN != newN {
			events = append(events, DetectedEvent{
				NaturalEventID: key + ":" + oldN + "->" + newN,
				SignalType:     "job_change",
				Title:          "Job change: " + contact.Name + " — " + prev + " → " + resolved,
				Description:    jobChangeDescription(contact, prev, resolved, evidence),
				Source:         jobChangeSource,
				SourceURL:      evidence,
				Weight:         JobChangeWeight,
				OccurredAt:     ts,
				LeadID:         contact.LeadID,
				Company:        resolved,
			})
			st["last_known_company"] = resolved
		}
		state[key] = st
	}

	if attempted > 0 && errored == attempted {
		return nil, nil, fmt.Errorf("%w: %w", ErrAllLookupsFailed, lastErr)
	}
	return events, map[string]any{JobChangeStateKey: state}, nil
}

// jobChangeDescription is json.dumps(payload) with Python's default separators
// and ensure_ascii, in Python's key order.
func jobChangeDescription(c Contact, prev, resolved, evidence string) string {
	str := func(s string) string {
		b, _ := canonjson.Marshal(s, false)
		return string(b)
	}
	lead := "null"
	if c.LeadID != nil {
		lead = strconv.FormatInt(*c.LeadID, 10)
	}
	return `{"lead_id": ` + lead + `, "person_name": ` + str(c.Name) + `, "old_company": ` + str(prev) +
		`, "new_company": ` + str(resolved) + `, "evidence_url": ` + str(evidence) + `}`
}
