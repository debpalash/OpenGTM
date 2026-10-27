package leadpeople

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// ExactEmailProviders is the discovery order; each only counts with an exact
// match method (prospeo: linkedin or exact_name, hunter_io: exact_name).
var ExactEmailProviders = []string{"prospeo", "hunter_io"}

var exactMethods = map[string]map[string]bool{
	"prospeo":   {"linkedin": true, "exact_name": true},
	"hunter_io": {"exact_name": true},
}

// DiscoveryResult is one provider's answer (the run_provider result dict).
type DiscoveryResult struct {
	Success    bool
	Fields     map[string]any
	Confidence float64
	DurationMS float64
	Error      string
	License    string
}

// EmailDiscoverer runs one email-finder provider for one lead. A nil result is
// a provider that is not available (Python None); an error wrapping
// context.DeadlineExceeded is a provider timeout, any other error is a failed
// attempt.
type EmailDiscoverer interface {
	Discover(ctx context.Context, workspaceID, provider string, lead map[string]any, timeout time.Duration) (*DiscoveryResult, error)
}

// EmailVerifier is verify_deliverability.
type EmailVerifier interface {
	Verify(ctx context.Context, workspaceID, email string) (emailverify.DeliverabilityResult, error)
}

// RunnerDiscoverer adapts providerrun.Runner (Go providers first, Python
// worker otherwise) to EmailDiscoverer.
type RunnerDiscoverer struct{ Runner *providerrun.Runner }

// Discover implements EmailDiscoverer.
func (r RunnerDiscoverer) Discover(ctx context.Context, ws, provider string, lead map[string]any, timeout time.Duration) (*DiscoveryResult, error) {
	out, err := r.Runner.Run(ctx, ws, provider, providers.Input{Lead: lead, Field: "email"}, timeout)
	if err != nil {
		if errors.Is(err, providerrun.ErrUnknownProvider) || errors.Is(err, providerrun.ErrUnavailable) {
			return nil, nil
		}
		return nil, err
	}
	if out.Error == "timeout" {
		return nil, context.DeadlineExceeded
	}
	res := &DiscoveryResult{Success: out.Error == "", Confidence: out.Confidence, Error: out.Error}
	if out.Extra != nil {
		res.Fields = map[string]any{}
		for _, k := range out.Extra.Keys() {
			v, _ := out.Extra.Get(k)
			res.Fields[k] = v
		}
	}
	return res, nil
}

// DepsVerifier adapts emailverify.Deps to EmailVerifier.
type DepsVerifier struct{ Deps emailverify.Deps }

// Verify implements EmailVerifier.
func (d DepsVerifier) Verify(ctx context.Context, ws, email string) (emailverify.DeliverabilityResult, error) {
	return emailverify.Deliverability(ctx, d.Deps, ws, email)
}

// safeDiscoveryFailure is _safe_discovery_failure.
func safeDiscoveryFailure(msg string) (status, detail string) {
	n := strings.ToLower(msg)
	switch {
	case strings.Contains(n, "api key") || strings.Contains(n, "configured"):
		return "unavailable", "provider_not_configured"
	case strings.Contains(n, "rate") || strings.Contains(n, "429"):
		return "error", "rate_limited"
	}
	return "no_data", "no_exact_email"
}

var emailRe = regexp.MustCompile(`^[^` + pyS + `@]+@[^` + pyS + `@]+\.[^` + pyS + `@]+$`)

// emailMatchesCompany is _email_matches_company.
func emailMatchesCompany(email, canonicalDomain string) bool {
	if !emailRe.MatchString(email) {
		return false
	}
	ed := NormalizeDomain(email[strings.LastIndexByte(email, '@')+1:])
	target := NormalizeDomain(canonicalDomain)
	return ed != "" && target != "" && (ed == target || strings.HasSuffix(ed, "."+target))
}

// publicContactStatus is _public_contact_status.
func publicContactStatus(v emailverify.DeliverabilityResult) string {
	switch {
	case v.Status == "invalid" || v.Confidence == "":
		return "invalid"
	case v.Status == "catch_all":
		return "catch_all"
	case v.Status == "valid" && v.Confidence == "verified":
		return "verified"
	}
	return "risky"
}

func license(provider, declared string) string { return workbook.ResolveLicense(provider, declared) }

// verificationAttempts is _verification_attempts.
func verificationAttempts(v emailverify.DeliverabilityResult) []map[string]any {
	if len(v.VerificationAttempts) > 0 {
		out := make([]map[string]any, 0, len(v.VerificationAttempts))
		for _, a := range v.VerificationAttempts {
			prov := a.Provider
			if prov == "" {
				prov = "verification_cascade"
			}
			status := a.Status
			if status == "" {
				status = "unknown"
			}
			out = append(out, map[string]any{
				"stage": "verification", "provider": prov, "status": status,
				"detail": trunc(a.Detail, 80), "source_license": license(a.Provider, ""),
			})
		}
		return out
	}
	prov := v.Source
	if prov == "" {
		prov = "verification_cascade"
	}
	status := v.Status
	if status == "" {
		status = "unknown"
	}
	return []map[string]any{{"stage": "verification", "provider": prov, "status": status, "detail": "",
		"source_license": license(v.Source, "")}}
}

func unavailableContactability(observedAt string, attempts []map[string]any) map[string]any {
	return map[string]any{
		"email": nil, "status": "unavailable", "finder_provider": "", "verifier_provider": "",
		"discovery_confidence": 0.0, "verification_status": "not_run", "observed_at": observedAt,
		"attempts": attempts, "exhausted": true,
	}
}

func (s *Service) enrichOne(ctx context.Context, person map[string]any, company, canonicalDomain, workspaceID string, timeout time.Duration) (map[string]any, error) {
	observedAt := isoFull(s.now())
	attempts := []map[string]any{}
	base := cloneMap(person)
	base["person_id"] = PersonEntityID(company, base)
	name := providers.PyStrip(firstStr(base, "name", "full_name"))

	withContact := func(email any, c map[string]any) map[string]any {
		out := cloneMap(base)
		out["email"] = email
		out["contactability"] = c
		return out
	}
	if canonicalDomain == "" {
		return withContact(nil, unavailableContactability(observedAt, []map[string]any{{
			"stage": "precondition", "provider": "company_identity", "status": "blocked",
			"detail": "canonical_company_unresolved", "source_license": "unknown",
		}})), nil
	}
	lead := map[string]any{
		"company": company, "website": "https://" + canonicalDomain, "contact_person": name,
		"contact_title": getStr(base, "title"), "linkedin_url": getStr(base, "linkedin_url"), "workspace_id": workspaceID,
	}
	var winner *DiscoveryResult
	winnerEmail, winnerProvider := "", ""
	for _, provider := range ExactEmailProviders {
		started := s.mono()
		since := func() int64 { return roundInt(float64(s.mono().Sub(started)) / float64(time.Millisecond)) }
		var res *DiscoveryResult
		var err error
		if s.Discover != nil {
			res, err = s.Discover.Discover(ctx, workspaceID, provider, lead, timeout)
		}
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			status, detail := "error", ErrorClass(err)
			if errors.Is(err, context.DeadlineExceeded) {
				status, detail = "timeout", "provider_timeout"
			}
			attempts = append(attempts, map[string]any{
				"stage": "discovery", "provider": provider, "status": status, "detail": detail,
				"duration_ms": since(), "source_license": license(provider, ""),
			})
			continue
		}
		measured := since()
		if res == nil {
			attempts = append(attempts, map[string]any{
				"stage": "discovery", "provider": provider, "status": "unavailable", "detail": "provider_unavailable",
				"duration_ms": measured, "source_license": license(provider, ""),
			})
			continue
		}
		duration := measured
		if res.DurationMS != 0 {
			duration = roundInt(res.DurationMS)
		}
		email := strings.ToLower(providers.PyStrip(getStr(res.Fields, "email")))
		if !res.Success || email == "" {
			status, detail := safeDiscoveryFailure(res.Error)
			attempts = append(attempts, map[string]any{
				"stage": "discovery", "provider": provider, "status": status, "detail": detail,
				"duration_ms": duration, "source_license": license(provider, res.License),
			})
			continue
		}
		method := strings.ToLower(providers.PyStrip(getStr(res.Fields, "email_match_method")))
		providerPerson := providers.PyStrip(getStr(res.Fields, "contact_person"))
		rejection := ""
		switch {
		case !exactMethods[provider][method]:
			rejection = "non_exact_match_method"
		case providerPerson != "" && identityText(providerPerson) != identityText(name):
			rejection = "person_identity_mismatch"
		case !emailMatchesCompany(email, canonicalDomain):
			rejection = "company_domain_mismatch"
		}
		if rejection != "" {
			attempts = append(attempts, map[string]any{
				"stage": "discovery", "provider": provider, "status": "rejected", "detail": rejection,
				"duration_ms": duration, "source_license": license(provider, res.License),
			})
			continue
		}
		attempts = append(attempts, map[string]any{
			"stage": "discovery", "provider": provider, "status": "found", "detail": method,
			"duration_ms": duration, "confidence": round4(res.Confidence), "source_license": license(provider, res.License),
		})
		winner, winnerEmail, winnerProvider = res, email, provider
		break
	}
	if winner == nil {
		return withContact(nil, unavailableContactability(observedAt, attempts)), nil
	}

	var verdict emailverify.DeliverabilityResult
	var verr error
	if s.Verify != nil {
		verdict, verr = s.Verify.Verify(ctx, workspaceID, winnerEmail)
	} else {
		verr = errors.New("no verifier configured")
	}
	if verr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		verdict = emailverify.DeliverabilityResult{Email: winnerEmail, Confidence: "unknown", Status: "unknown", Source: "verification_cascade"}
		attempts = append(attempts, map[string]any{
			"stage": "verification", "provider": "verification_cascade", "status": "error",
			"detail": ErrorClass(verr), "source_license": "unknown",
		})
	} else {
		attempts = append(attempts, verificationAttempts(verdict)...)
	}
	status := publicContactStatus(verdict)
	var email any = winnerEmail
	if status == "invalid" {
		email = nil
	}
	verifier := verdict.Source
	if verifier == "" {
		verifier = "verification_cascade"
	}
	vstatus := verdict.Status
	if vstatus == "" {
		vstatus = "unknown"
	}
	return withContact(email, map[string]any{
		"email": email, "status": status, "finder_provider": winnerProvider, "verifier_provider": verifier,
		"discovery_confidence": round4(winner.Confidence), "verification_status": vstatus,
		"verification_confidence": round4(verdict.VerificationConf), "observed_at": observedAt,
		"attempts": attempts, "exhausted": false,
		"is_role": verdict.IsRole, "is_free": verdict.IsFree, "is_disposable": verdict.IsDisposable,
	}), nil
}

// ContactOptions are the keyword arguments of enrich_people_contacts.
type ContactOptions struct {
	CompanyResolution map[string]any
	PersonIDs         []string
	WorkspaceID       string
	ActionID          string
	ProviderTimeout   time.Duration // 0 = 20s; clamped to 1s..45s
}

// EnrichPeopleContacts is enrich_people_contacts: exact-person work-email
// discovery followed by an independent deliverability check, for an exact saved
// people selection. It never searches for replacement people and a finder claim
// alone never receives verified status. Business failures (unknown selection,
// nothing selected) are {"ok": false, ...} results; the error is non-nil only
// when ctx ends.
func (s *Service) EnrichPeopleContacts(ctx context.Context, company, function string, people []map[string]any, o ContactOptions) (map[string]any, error) {
	company = trunc(clean(company), 120)
	function = trunc(clean(function), 100)
	resolution := cloneMap(o.CompanyResolution)
	canonicalDomain := NormalizeDomain(providers.PyStrip(getStr(resolution, "canonical_domain")))
	var candidates []map[string]any
	byID := map[string]map[string]any{}
	for _, raw := range people {
		if raw == nil {
			continue
		}
		person := cloneMap(raw)
		id := PersonEntityID(company, person)
		person["person_id"] = id
		if canonicalDomain == "" {
			canonicalDomain = NormalizeDomain(providers.PyStrip(getStr(person, "canonical_company_domain")))
		}
		if _, dup := byID[id]; !dup {
			candidates = append(candidates, person)
			byID[id] = person
		}
	}
	var requested []string
	seen := map[string]bool{}
	for _, id := range o.PersonIDs {
		if id = providers.PyStrip(id); id != "" && !seen[id] {
			seen[id] = true
			requested = append(requested, id)
		}
	}
	var unknown []string
	for _, id := range requested {
		if _, ok := byID[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return map[string]any{"ok": false, "error": "Unknown people selection", "unknown_person_ids": unknown}, nil
	}
	selected := candidates
	if len(requested) > 0 {
		selected = selected[:0:0]
		for _, id := range requested {
			selected = append(selected, byID[id])
		}
	}
	if len(selected) == 0 {
		return map[string]any{"ok": false, "error": "No people selected", "people": []any{}, "count": 0}, nil
	}
	selectedIDs := make([]string, len(selected))
	for i, p := range selected {
		selectedIDs[i] = p["person_id"].(string)
	}

	timeout := o.ProviderTimeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	timeout = time.Duration(math.Max(1, math.Min(timeout.Seconds(), 45)) * float64(time.Second))
	enriched := make([]map[string]any, len(selected))
	errs := make([]error, len(selected))
	sem := make(chan struct{}, s.concurrency())
	var wg sync.WaitGroup
	for i, p := range selected {
		sem <- struct{}{} // acquired in order, like the Python semaphore (FIFO)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			enriched[i], errs[i] = s.enrichOne(ctx, p, company, canonicalDomain, o.WorkspaceID, timeout)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	summary := map[string]any{}
	for _, st := range []string{"verified", "risky", "catch_all", "invalid", "unavailable"} {
		n := 0
		for _, p := range enriched {
			if c, _ := p["contactability"].(map[string]any); c["status"] == st {
				n++
			}
		}
		summary[st] = n
	}
	resultSetID := PeopleResultSetID(company, function, candidates)
	actionID := o.ActionID
	if actionID == "" {
		sorted := append([]string(nil), selectedIDs...)
		sort.Strings(sorted)
		sum := sha256.Sum256([]byte(strings.Join(sorted, "|")))
		actionID = "people-contacts:" + resultSetID + ":" + hex.EncodeToString(sum[:])[:16]
	}
	return map[string]any{
		"ok": true, "action_id": actionID, "reused": false, "workspace_id": o.WorkspaceID, "company": company,
		"function": function, "company_resolution": resolution, "canonical_company_domain": canonicalDomain,
		"result_set_id": resultSetID, "selected_person_ids": selectedIDs, "provider_order": ExactEmailProviders,
		"people": enriched, "count": len(enriched), "summary": summary, "observed_at": isoFull(s.now()),
		"methodology": "Exact-person email discovery followed by an independent deliverability check. " +
			"Finder claims alone never receive verified status.",
	}, nil
}
