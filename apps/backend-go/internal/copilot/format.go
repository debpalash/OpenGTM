package copilot

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// The Format* functions are formatting.py: deterministic, evidence-honest Chat renderers over a tool
// result decoded as map[string]any (json.Number or plain Go numbers both work). Python truthiness and
// str() are reproduced by pyTruthy and pyStr.

func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int32:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 64)
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case *wbcrud.Obj:
		return x != nil && x.Len() > 0
	}
	return true
}

// pyStr is str(v) for JSON values.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(x)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return pyFloat(x)
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return pyFloat(f)
			}
			return s
		}
		if s == "-0" {
			return "0"
		}
		return s
	}
	return pyRepr(v)
}

func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s, err := canonjson.FloatRepr(f)
	if err != nil {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return s
}

// pyRepr is repr(v) for containers (a best-effort match for str(list)/str(dict)).
func pyRepr(v any) string {
	switch x := v.(type) {
	case string:
		q := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			q = `"`
		}
		r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
		s := r.Replace(x)
		if q == "'" {
			s = strings.ReplaceAll(s, "'", `\'`)
		}
		return q + s + q
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // decoded maps carry no order
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyRepr(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyStr(v)
}

// or is Python's `a or b`.
func or(v any, dflt string) string {
	if pyTruthy(v) {
		return pyStr(v)
	}
	return dflt
}

// pyFloatOf is float(v or 0): numbers, numeric strings and booleans; anything else counts as 0 (Python
// would raise ValueError, which would abort the turn).
func pyFloatOf(v any) float64 {
	if !pyTruthy(v) {
		return 0
	}
	switch x := v.(type) {
	case bool:
		return 1
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	case json.Number:
		f, _ := strconv.ParseFloat(x.String(), 64)
		return f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		return f
	}
	return 0
}

// percent is f"{round(float(v or 0) * 100)}%" (round half to even, like Python's round).
func percent(v any) string {
	r := math.RoundToEven(pyFloatOf(v) * 100)
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return pyFloat(r) + "%"
	}
	return fmt.Sprintf("%.0f%%", r)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// asList is `result.get(key) or []`.
func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func esc(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func cell(v any) string {
	return strings.ReplaceAll(esc(or(v, "—")), "\n", " ")
}

func first(vals ...any) any {
	for _, v := range vals {
		if pyTruthy(v) {
			return v
		}
	}
	return nil
}

// count is summary.get(key, 0) interpolated into an f-string.
func count(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		return pyStr(v)
	}
	return "0"
}

func get(m map[string]any, key, dflt string) string {
	if v, ok := m[key]; ok {
		return pyStr(v)
	}
	return dflt
}

// FormatPeopleResearch renders find_people_at_company results.
func FormatPeopleResearch(result map[string]any) string {
	company := or(result["company"], "the target company")
	function := or(result["function"], "requested")
	people := asList(result["people"])
	if len(people) == 0 {
		rej := asMap(result["candidates_rejected"])
		return "I couldn't verify any **" + function + "** people at **" + company + "** from the " +
			"public profile evidence available right now. I rejected " +
			count(rej, "company_relationship_missing") + " candidate(s) without an " +
			"exact company relationship and " +
			count(rej, "function_evidence_missing") + " without function evidence. " +
			"No broad collection was run and no contacts were guessed."
	}
	rows := make([]string, 0, len(people))
	for _, pv := range people {
		p := asMap(pv)
		name := cell(p["name"])
		linkedin := or(first(p["linkedin_url"], p["evidence_url"]), "")
		personCell := name
		if linkedin != "" {
			personCell = "[" + name + "](" + linkedin + ")"
		}
		rows = append(rows, "| "+personCell+" | "+cell(p["title"])+" | "+cell(p["location"])+" | "+
			cell(p["retrieved_at"])+" | "+percent(p["confidence"])+" |")
	}
	return "I found **" + strconv.Itoa(len(people)) + " evidence-matched candidate(s)** for **" + company + " — " +
		function + "**.\n\n" +
		"| Person | Public title | Location | Retrieved | Confidence |\n" +
		"|---|---|---|---|---|\n" +
		strings.Join(rows, "\n") +
		"\n\nEvery row passed an exact target-company employment pattern and function-term " +
		"check. Public profile snippets can still be stale, so re-check the linked profile " +
		"before outreach. No email addresses were inferred."
}

// FormatPeopleVerification renders verify_people_at_company results.
func FormatPeopleVerification(result map[string]any) string {
	people := asList(result["people"])
	summary := asMap(result["summary"])
	if len(people) == 0 {
		return "There are no saved people candidates in this conversation to verify."
	}
	labels := map[string]string{
		"independent_role_evidence": "Independent role evidence",
		"profile_reconfirmed":       "Profile reconfirmed",
		"not_corroborated":          "Not corroborated",
		"verification_timeout":      "Timed out",
	}
	rows := make([]string, 0, len(people))
	for _, pv := range people {
		p := asMap(pv)
		name := esc(or(p["name"], "—"))
		url := or(first(p["linkedin_url"], p["evidence_url"]), "")
		nameCell := name
		if url != "" {
			nameCell = "[" + name + "](" + url + ")"
		}
		title := esc(or(p["title"], "—"))
		status := or(p["verification_status"], "Unknown")
		if s, ok := p["verification_status"].(string); ok {
			if l, found := labels[s]; found {
				status = l
			}
		}
		var links []string
		for i, sv := range asList(p["verification_sources"]) {
			if i >= 3 {
				break
			}
			if u, ok := asMap(sv)["url"]; ok && pyTruthy(u) {
				links = append(links, fmt.Sprintf("[source %d](%s)", i+1, pyStr(u)))
			}
		}
		joined := "—"
		if len(links) > 0 {
			joined = strings.Join(links, ", ")
		}
		rows = append(rows, "| "+nameCell+" | "+title+" | "+status+" | "+joined+" | "+percent(p["verification_confidence"])+" |")
	}
	return "I re-checked **" + strconv.Itoa(len(people)) + " people** for **" + get(result, "company", "the company") + " — " +
		get(result, "function", "the requested function") + "** using fresh public searches.\n\n" +
		"| Person | Public title | Verification | Fresh evidence | Confidence |\n" +
		"|---|---|---|---|---|\n" +
		strings.Join(rows, "\n") +
		"\n\n" +
		"**Summary:** " + count(summary, "independent_role_evidence") + " with independent role evidence, " +
		count(summary, "profile_reconfirmed") + " profile-reconfirmed, " +
		count(summary, "not_corroborated") + " not corroborated. “Not corroborated” is not a " +
		"negative finding—it means I did not find fresh supporting evidence. This verifies role " +
		"evidence, not email or phone ownership."
}

// FormatPeopleContacts renders enrich_people_contacts results.
func FormatPeopleContacts(result map[string]any) string {
	people := asList(result["people"])
	summary := asMap(result["summary"])
	if len(people) == 0 {
		return "No saved people were available for contact enrichment."
	}
	labels := map[string]string{
		"verified": "Verified", "risky": "Risky", "catch_all": "Catch-all",
		"invalid": "Invalid", "unavailable": "Unavailable",
	}
	rows := make([]string, 0, len(people))
	for _, pv := range people {
		p := asMap(pv)
		contact := asMap(p["contactability"])
		name := esc(or(first(p["name"], p["full_name"]), "Unknown"))
		email := esc(or(contact["email"], "Not found"))
		status := "Unavailable"
		if s, ok := contact["status"].(string); ok {
			if l, found := labels[s]; found {
				status = l
			}
		}
		finder := esc(or(contact["finder_provider"], "None"))
		verifier := esc(or(contact["verifier_provider"], "Not run"))
		rows = append(rows, "| "+name+" | "+email+" | "+status+" | "+finder+" | "+verifier+" |")
	}
	return "Checked work-email contactability for **" + strconv.Itoa(len(people)) + " saved people** at " +
		"**" + get(result, "company", "the target company") + "**.\n\n" +
		"| Person | Work email | Status | Finder | Verifier |\n" +
		"|---|---|---|---|---|\n" +
		strings.Join(rows, "\n") +
		"\n\n" +
		"Summary: " + count(summary, "verified") + " verified, " +
		count(summary, "catch_all") + " catch-all, " +
		count(summary, "risky") + " risky, " +
		count(summary, "invalid") + " invalid, and " +
		count(summary, "unavailable") + " unavailable. " +
		"Verified means a separate verifier returned a valid mailbox result. " +
		"Catch-all and risky addresses still need caution before outreach."
}

// FormatSignalTracking renders track_account_signals results.
func FormatSignalTracking(result map[string]any) string {
	scope := asMap(result["scope"])
	labels := map[string]string{
		"partnership_hiring":  "partnership hiring",
		"leadership_change":   "leadership changes",
		"funding":             "funding",
		"pricing_page_change": "pricing-page changes",
	}
	var signals []string
	for _, v := range asList(result["signal_types"]) {
		if s, ok := v.(string); ok {
			if l, found := labels[s]; found {
				signals = append(signals, l)
				continue
			}
		}
		signals = append(signals, pyStr(v))
	}
	if !pyTruthy(result["ok"]) {
		return "I could not save the tracking schedule: " + get(result, "error", "unknown error") + "."
	}
	return "Tracking is **" + get(result, "state", "active") + "** for " +
		"**" + get(scope, "account_count", "0") + " exact accounts** on a " +
		"**" + get(result, "cadence", "weekly") + "** cadence. " +
		"Collectors: " + strings.Join(signals, ", ") + ". " +
		"Next run: " + or(result["next_run_at"], "not scheduled") + ". " +
		"I read schedule `" + pyStr(result["schedule_id"]) + "` back from saved state before confirming it. " +
		"[Open tracking](" + pyStr(result["url"]) + ")"
}

// FormatGroundedDraft renders draft_grounded_outreach results.
func FormatGroundedDraft(result map[string]any) string {
	if !pyTruthy(result["ok"]) {
		return "I could not create the draft: " + get(result, "error", "unknown error") + ". " +
			"No message was sent."
	}
	return "Saved a **draft only** for **" + pyStr(result["person_name"]) + "** " +
		"at **" + pyStr(result["company"]) + "**. No message was sent.\n\n" +
		"**To:** " + pyStr(result["to_email"]) + "  \n" +
		"**Subject:** " + pyStr(result["subject"]) + "\n\n" +
		pyStr(result["body_text"]) + "\n\n" +
		"The " + get(result, "personalized_sentence_count", "0") + " personalized lines retain " +
		"their saved evidence. [Inspect draft and evidence](" + pyStr(result["url"]) + ")"
}
