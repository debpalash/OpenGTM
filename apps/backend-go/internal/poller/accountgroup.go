package poller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// Collectors for an exact group of persisted workbook accounts
// (account_group.py): partnership hiring (JobSpy through the pyworker RPC),
// SEC funding/leadership (Task 11) and a pricing-page fingerprint over the
// pinned HTTP client. One failing collector is that collector's failure
// (_collector_failures + collector_health), never an exception that aborts the
// others.

const (
	pricingTimeout      = 12 * time.Second
	pricingMaxBody      = 1 << 20
	pricingUA           = "OpenGTM Signal Monitor/1.0"
	fingerprintMaxRunes = 500_000
	partnershipWeight   = 8
	pricingWeight       = 8
	maxPartnershipIDs   = 500
)

var partnershipTerms = []string{
	"partnership", "partnerships", "alliances", "partner development",
	"partner ecosystem", "channel partner", "business development",
}

var (
	// (?i)<script\b[^>]*>.*?</script> with S: Python's \b is Unicode, so the
	// boundary after "script" is spelled as "next char is not a word char".
	agScriptRE = regexp.MustCompile(`(?is)<scr` + pcI + `pt(?:>|[^>\p{L}\p{N}_][^>]*>).*?</scr` + pcI + `pt>`)
	agStyleRE  = regexp.MustCompile(`(?is)<style(?:>|[^>\p{L}\p{N}_][^>]*>).*?</style>`)
	agTagRE    = regexp.MustCompile(`<[^>]+>`)
)

// FingerprintHTML is _fingerprint_html: volatile markup stripped, whitespace
// collapsed, lowercased, the first 500,000 characters hashed (SHA-256 hex).
// The hash is persisted, never the page body.
func FingerprintHTML(text string) string {
	v := agScriptRE.ReplaceAllString(text, " ")
	v = agStyleRE.ReplaceAllString(v, " ")
	v = agTagRE.ReplaceAllString(v, " ")
	v = pcLower(providers.PyStrip(jcSpaceRE.ReplaceAllString(v, " ")))
	if utf8.RuneCountInString(v) > fingerprintMaxRunes {
		v = string([]rune(v)[:fingerprintMaxRunes])
	}
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// agObservation is one collector's observation. OK must be exactly true for a
// healthy collector.
type agObservation struct {
	OK         bool
	ErrorClass string
	// partnership_hiring
	Items []agItem
	// funding / leadership_change
	Events      []DetectedEvent
	Cursor      map[string]any
	ResolvedCIK string
	// pricing_page_change
	Fingerprint string
	URL         string
}

type agItem struct{ ID, Title, URL string }

func agFail(class string) agObservation { return agObservation{ErrorClass: class} }

// PartnershipJobs is _partnership_jobs over the jobspy_signals RPC. A pyworker
// outage, timeout or provider error is a failed observation, not an error.
func PartnershipJobs(ctx context.Context, d PyDeps, w PyWatch, idx int, company string) agObservation {
	fields, err := d.Py.pcCall(ctx, w, fmt.Sprintf("account_group_hiring_%d", idx), 0, jobspyProvider,
		map[string]any{"company": company}, d.Py.jobSpyTimeout())
	if err != nil {
		var pe *PyCallError
		switch {
		case errors.As(err, &pe) && pe.Msg == "no_jobs_found":
			return agObservation{OK: true}
		case errors.Is(err, ErrPyTimeout):
			return agFail("hiring_provider_error")
		default:
			return agFail("hiring_provider_unavailable")
		}
	}
	raw, _ := fields["hiring_signals"].(string)
	if raw == "" {
		raw = "{}"
	}
	v, perr := providers.ParseJSON([]byte(raw))
	payload, isMap := v.(map[string]any)
	if perr != nil || !isMap {
		return agFail("hiring_payload_invalid")
	}
	items := []agItem{}
	roles, _ := payload["roles"].([]any)
	for _, r := range roles {
		title := providers.PyStrip(providers.Str(r))
		if title == "" {
			continue
		}
		lower := pcLower(title)
		for _, term := range partnershipTerms {
			if strings.Contains(lower, term) {
				sum := sha256.Sum256([]byte(lower))
				items = append(items, agItem{ID: hex.EncodeToString(sum[:])[:20], Title: title})
				break
			}
		}
	}
	return agObservation{OK: true, Items: items}
}

// FetchPricing is _fetch_pricing: GET the pricing page over the pinned egress
// client and fingerprint its visible text. Deviations from Python, all
// hardening (plan 08 D4): redirects are never followed (a 3xx is
// pricing_http_<code>), the body is read up to 1 MiB (the rest is ignored), and
// the 12 s deadline covers the whole request.
func FetchPricing(ctx context.Context, d PyDeps, rawURL string) agObservation {
	if err := egress.CheckURLFor(d.AppEnv, d.EgressAllowHosts, rawURL, true); err != nil || numericHost(rawURL) {
		return agFail("pricing_url_blocked")
	}
	rctx, cancel := context.WithTimeout(ctx, pricingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return agFail("pricing_url_blocked")
	}
	req.Header.Set("User-Agent", pricingUA)
	c := *d.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return agFail(pricingTransportClass(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return agObservation{OK: true, URL: rawURL, Fingerprint: "missing"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return agFail(fmt.Sprintf("pricing_http_%d", resp.StatusCode))
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(ct), "html") {
		return agFail("pricing_not_html")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, pricingMaxBody))
	if err != nil {
		return agFail(pricingTransportClass(err))
	}
	return agObservation{OK: true, URL: normalizedURL(rawURL), Fingerprint: FingerprintHTML(decodePricingBody(body, ct))}
}

func pricingTransportClass(err error) string {
	if errors.Is(err, egress.ErrBlocked) {
		return "pricing_url_blocked"
	}
	if _, ok := blockedText(err.Error()); ok {
		return "pricing_url_blocked"
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "pricing_timeout"
	}
	return "pricing_transport_error"
}

// numericHost reports a host whose last label is numeric ("127.1", "0x7f.1"):
// an IPv4 literal in a short or odd encoding. No registered TLD is numeric, and
// the Go resolver would not read these as addresses, so they are refused outright
// (the Python guard parses them as IPs and blocks the private ones).
func numericHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.TrimSuffix(u.Hostname(), ".")
	if _, err := netip.ParseAddr(h); err == nil {
		return false // a full literal is the egress guard's to judge
	}
	last := h[strings.LastIndex(h, ".")+1:]
	if last == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(last), "0x") {
		return true
	}
	return last[0] >= '0' && last[0] <= '9'
}

// normalizedURL is str(response.url): lowercase scheme and host.
func normalizedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

// decodePricingBody is response.text: the Content-Type charset (UTF-8 when
// absent or unknown), invalid bytes replaced.
func decodePricingBody(body []byte, contentType string) string {
	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		if label := params["charset"]; label != "" {
			switch strings.ToLower(strings.TrimSpace(label)) {
			case "latin1", "latin-1", "iso-8859-1", "iso8859-1", "iso_8859-1", "l1":
				// Python's latin-1 is a byte-to-code-point identity (the WHATWG
				// table behind charset.Lookup would map these to windows-1252).
				rs := make([]rune, len(body))
				for i, b := range body {
					rs[i] = rune(b)
				}
				return string(rs)
			}
			if enc, _ := charset.Lookup(label); enc != nil {
				if out, err := enc.NewDecoder().Bytes(body); err == nil {
					return strings.ToValidUTF8(string(out), "�")
				}
			}
		}
	}
	return strings.ToValidUTF8(string(body), "�")
}

// agObserve produces the observations of one account for the requested signal
// types. A non-nil error is a cancelled context only.
type agObserve func(ctx context.Context, idx int, account, accountCursor map[string]any, requested map[string]bool) (map[string]agObservation, error)

func agStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		return providers.Str(v)
	}
	return ""
}

// FetchAccountGroup is fetch_account_group: it diffs every requested collector
// for every exact account. Patch keys: account_group, collector_health,
// _collector_failures (the error classes of this poll's failed collectors).
func FetchAccountGroup(ctx context.Context, d PyDeps, w PyWatch, backfill bool) ([]DetectedEvent, map[string]any, error) {
	observe := func(ctx context.Context, idx int, account, ac map[string]any, requested map[string]bool) (map[string]agObservation, error) {
		out := map[string]agObservation{}
		company := agStr(account, "company")
		if requested["partnership_hiring"] {
			out["partnership_hiring"] = PartnershipJobs(ctx, d, w, idx, company)
		}
		if requested["funding"] || requested["leadership_change"] {
			sec, err := agSEC(ctx, d, w, account, ac, backfill)
			if err != nil {
				return nil, err
			}
			for sig, srcType := range map[string]string{"funding": "company_funded", "leadership_change": "executive_hired"} {
				if !requested[sig] {
					continue
				}
				if !sec.OK {
					out[sig] = sec
					continue
				}
				o := sec
				o.Events = nil
				for _, e := range sec.Events {
					if e.SignalType == srcType {
						o.Events = append(o.Events, e)
					}
				}
				out[sig] = o
			}
		}
		if requested["pricing_page_change"] {
			site := ""
			if s, ok := account["website"].(string); ok {
				site = s
			}
			out["pricing_page_change"] = FetchPricing(ctx, d, strings.TrimRight(site, "/")+"/pricing")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return out, nil
	}
	return fetchAccountGroupWith(ctx, w, backfill, d.epochNow(), observe)
}

// agSEC is _sec_events: the SEC detector (Task 11) over a virtual per-account
// watch carrying the account's own cursor slice.
func agSEC(ctx context.Context, d PyDeps, w PyWatch, account, ac map[string]any, backfill bool) (agObservation, error) {
	cursor := copyMap(asMap(ac["sec"]))
	cursor["bootstrapped"] = providers.Truthy(ac["bootstrapped"])
	cik := ""
	if s, ok := ac["resolved_cik"].(string); ok {
		cik = s
	}
	virtual := WatchView{
		ID:          w.ID + ":" + agStr(account, "account_id"),
		Target:      agStr(account, "company"),
		Cursor:      cursor,
		ResolvedCIK: cik,
	}
	events, patch, err := FetchFundingAndExec(ctx, d.SourceDeps, virtual, true, true, backfill)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return agObservation{}, cerr
		}
		return agFail("sec_provider_unavailable"), nil
	}
	patch = copyMap(patch)
	resolved := cik
	if rc, ok := patch["_resolved_cik"].(string); ok && rc != "" {
		resolved = rc
	}
	delete(patch, "_resolved_cik")
	return agObservation{OK: true, Events: events, Cursor: patch, ResolvedCIK: resolved}, nil
}

func fetchAccountGroupWith(ctx context.Context, w PyWatch, backfill bool, occurredAt float64, observe agObserve) ([]DetectedEvent, map[string]any, error) {
	accounts, _ := w.Config["accounts"].([]any)
	requested := map[string]bool{}
	for _, s := range w.SignalTypes {
		requested[s] = true
	}
	sorted := make([]string, 0, len(requested))
	for s := range requested {
		sorted = append(sorted, s)
	}
	sort.Strings(sorted)

	groupCursor := copyMap(asMap(w.Cursor["account_group"]))
	health := copyMap(asMap(w.Cursor["collector_health"]))
	failures := []any{}
	emitted := []DetectedEvent{}

	for idx, a := range accounts {
		account := asMap(a)
		if account == nil || !providers.Truthy(account["account_id"]) {
			continue
		}
		accountID := providers.Str(account["account_id"])
		company := agStr(account, "company")
		accountCursor := copyMap(asMap(groupCursor[accountID]))
		bootstrapped := providers.Truthy(accountCursor["bootstrapped"])
		suppress := !bootstrapped && !backfill

		observed, err := observe(ctx, idx, account, accountCursor, requested)
		if err != nil {
			return nil, nil, err
		}

		knownJobs, _ := accountCursor["partnership_job_ids"].([]any)
		knownJobs = append([]any{}, knownJobs...)
		knownSet := map[string]bool{}
		for _, k := range knownJobs {
			if s, ok := k.(string); ok {
				knownSet[s] = true
			}
		}
		lead := leadIDOf(account["lead_id"])

		for _, signalType := range sorted {
			key := accountID + ":" + signalType
			prior := asMap(health[key])
			attempt := int64(1)
			if n, ok := pcInt(prior["attempt_count"]); ok && providers.Truthy(prior["attempt_count"]) {
				attempt = n + 1
			}
			obs := observed[signalType]
			if !obs.OK {
				class := obs.ErrorClass
				if class == "" {
					class = "collector_unavailable"
				}
				failures = append(failures, class)
				health[key] = map[string]any{"state": "failed", "attempt_count": attempt, "last_error_class": class}
				continue
			}
			health[key] = map[string]any{"state": "healthy", "attempt_count": attempt, "last_error_class": nil}

			switch signalType {
			case "partnership_hiring":
				for _, item := range obs.Items {
					id := providers.PyStrip(item.ID)
					if id == "" || knownSet[id] {
						continue
					}
					knownSet[id] = true
					knownJobs = append(knownJobs, id)
					if suppress {
						continue
					}
					title := item.Title
					if title == "" {
						title = "Untitled role"
					}
					emitted = append(emitted, DetectedEvent{
						NaturalEventID: accountID + ":partnership_job:" + id,
						SignalType:     "partnership_hiring",
						Title:          "Partnership role opened: " + title,
						Description:    "New partnership hiring evidence for " + company,
						Source:         "jobspy",
						SourceURL:      item.URL,
						Weight:         partnershipWeight,
						OccurredAt:     occurredAt,
						LeadID:         lead,
						Company:        company,
					})
				}
			case "funding", "leadership_change":
				srcSignal := "executive_hired"
				if signalType == "funding" {
					srcSignal = "company_funded"
				}
				for _, ev := range obs.Events {
					if ev.SignalType != srcSignal || suppress {
						continue
					}
					emitted = append(emitted, DetectedEvent{
						NaturalEventID: accountID + ":" + ev.NaturalEventID,
						SignalType:     signalType,
						Title:          ev.Title,
						Description:    ev.Description,
						Source:         ev.Source,
						SourceURL:      ev.SourceURL,
						Weight:         ev.Weight,
						OccurredAt:     ev.OccurredAt,
						LeadID:         lead,
						Company:        company,
					})
				}
				if len(obs.Cursor) > 0 {
					accountCursor["sec"] = copyMap(obs.Cursor)
				}
				if obs.ResolvedCIK != "" {
					accountCursor["resolved_cik"] = obs.ResolvedCIK
				}
			case "pricing_page_change":
				fp := obs.Fingerprint
				previous, _ := accountCursor["pricing_fingerprint"].(string)
				if fp != "" {
					accountCursor["pricing_fingerprint"] = fp
					accountCursor["pricing_url"] = obs.URL
				}
				if previous != "" && fp != "" && previous != fp && !suppress {
					src := obs.URL
					if src == "" {
						src = agStr(account, "website")
					}
					n := len(fp)
					if n > 20 {
						n = 20
					}
					emitted = append(emitted, DetectedEvent{
						NaturalEventID: accountID + ":pricing:" + fp[:n],
						SignalType:     "pricing_page_change",
						Title:          "Pricing page changed",
						Description:    "The saved pricing-page fingerprint changed for " + company,
						Source:         "company_website",
						SourceURL:      src,
						Weight:         pricingWeight,
						OccurredAt:     occurredAt,
						LeadID:         lead,
						Company:        company,
					})
				}
			}
		}

		if len(knownJobs) > maxPartnershipIDs {
			knownJobs = knownJobs[len(knownJobs)-maxPartnershipIDs:]
		}
		accountCursor["partnership_job_ids"] = knownJobs
		accountCursor["bootstrapped"] = true
		groupCursor[accountID] = accountCursor
	}

	return emitted, map[string]any{
		"account_group":       groupCursor,
		"collector_health":    health,
		"_collector_failures": failures,
	}, nil
}

// leadIDOf is account.get("lead_id") as a routing override: integers only.
func leadIDOf(v any) *int64 {
	switch t := v.(type) {
	case int64:
		return &t
	case int:
		n := int64(t)
		return &n
	case float64:
		if t == float64(int64(t)) {
			n := int64(t)
			return &n
		}
	}
	return nil
}
