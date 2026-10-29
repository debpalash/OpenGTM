package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
)

// DetectedEvent is sources.DetectedEvent: one proposed signal. LeadID and
// Company are per-event routing overrides (nil / "" = the watch's own values).
type DetectedEvent struct {
	SignalType     string
	NaturalEventID string
	Title          string
	Description    string
	Source         string
	SourceURL      string
	Weight         float64
	OccurredAt     float64
	LeadID         *int64
	Company        string
}

// WatchView is the slice of a watch the HTTP detectors read. Cursor is the
// decoded watch cursor (nil = none).
type WatchView struct {
	ID          string
	Target      string
	Cursor      map[string]any
	ResolvedCIK string
}

// SourceDeps are the dependencies of the HTTP sources. Deps carries the clock
// and Config (WebsiteTech gates the web_tech source). The zero value of every
// other field is the production default.
type SourceDeps struct {
	Deps
	// Client is the egress-guarded client (nil: egress.NewClient(nil)). It MUST
	// be an egress client: redirects are never followed and every dial is vetted.
	Client *http.Client
	// SecUserAgent overrides the SEC User-Agent (SEC_EDGAR_USER_AGENT).
	SecUserAgent string
	// FeedMaxEntries is INTENT_POLLER_FEED_MAX_ENTRIES (0: 100).
	FeedMaxEntries int
	// Spacer is the per-host feed spacer (nil: 1 request/second/host).
	Spacer *HostSpacer

	// Test seams (nil: production).
	FeedFetch func(ctx context.Context, d SourceDeps, url, etag, lastModified string) (FeedResult, error)
	FormD     func(ctx context.Context, target, since string) ([]firmo.SecFiling, error)
	TechStack providers.Provider
}

var (
	defaultClientOnce sync.Once
	defaultClient     *http.Client
)

func (d SourceDeps) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	defaultClientOnce.Do(func() { defaultClient = egress.NewClient(nil) })
	return defaultClient
}

func (d SourceDeps) spacer() *HostSpacer {
	if d.Spacer != nil {
		return d.Spacer
	}
	return defaultSpacer
}

func (d SourceDeps) feedMax() int {
	if d.FeedMaxEntries > 0 {
		return d.FeedMaxEntries
	}
	return 100
}

func (d SourceDeps) epochNow() float64 {
	return float64(d.now().UnixNano()) / 1e9
}

// ── cursor helpers (Python dict.get semantics over decoded JSON) ────────────

func cursorGet(w WatchView, key string) any {
	if w.Cursor == nil {
		return nil
	}
	return w.Cursor[key]
}

func cursorStrings(w WatchView, key string) []string {
	v := cursorGet(w, key)
	var out []string
	switch t := v.(type) {
	case []string:
		out = append(out, t...)
	case []any:
		for _, e := range t {
			out = append(out, providers.Str(e))
		}
	}
	return out
}

func cursorFloat(w WatchView, key string) float64 {
	switch t := cursorGet(w, key).(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case int:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func cursorString(w WatchView, key string) string {
	if s, ok := cursorGet(w, key).(string); ok {
		return s
	}
	return ""
}

func (w WatchView) suppress(backfill bool) bool {
	return !providers.Truthy(cursorGet(w, "bootstrapped")) && !backfill
}

func (w WatchView) stableID() string {
	var rc *string
	if w.ResolvedCIK != "" {
		rc = &w.ResolvedCIK
	}
	return StableCompanyID(rc, w.Target)
}

// ── Funding (SEC Form D) + executive appointments ───────────────────────────

var secDateStrictRE = regexp.MustCompile(`^(\d{4})-(1[0-2]|0[1-9]|[1-9])-(3[01]|[12]\d|0[1-9]|[1-9]| [1-9])$`)

// epochFromDate is _epoch_from_date: strptime("%Y-%m-%d") in UTC; blank or
// unparseable is now.
func (d SourceDeps) epochFromDate(s string) float64 {
	s = providers.PyStrip(s)
	if s == "" {
		return d.epochNow()
	}
	m := secDateStrictRE.FindStringSubmatch(s)
	if m == nil {
		return d.epochNow()
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(strings.TrimSpace(m[3]))
	if y < 1 {
		return d.epochNow()
	}
	t := time.Date(y, time.Month(mo), day, 0, 0, 0, 0, time.UTC)
	if t.Day() != day {
		return d.epochNow()
	}
	return float64(t.Unix())
}

func (d SourceDeps) listFormD(ctx context.Context, target, since string) ([]firmo.SecFiling, error) {
	if d.FormD != nil {
		return d.FormD(ctx, target, since)
	}
	return firmo.SecListFormDSince(ctx, d.client(), d.SecUserAgent, target, since)
}

// formatInt is f"{int(amount):,}" for the shapes a Form D amount takes.
func formatAmount(v any) (string, error) {
	n := new(big.Int)
	switch t := v.(type) {
	case int64:
		n.SetInt64(t)
	case int:
		n.SetInt64(int64(t))
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return "", errors.New("cannot convert float to integer")
		}
		bf := new(big.Float).SetFloat64(math.Trunc(t))
		bf.Int(n)
	case json.Number:
		if _, ok := n.SetString(string(t), 10); !ok {
			f, err := t.Float64()
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return "", errors.New("invalid amount")
			}
			new(big.Float).SetFloat64(math.Trunc(f)).Int(n)
		}
	case string:
		if _, ok := n.SetString(providers.PyStrip(t), 10); !ok {
			return "", fmt.Errorf("invalid literal for int() with base 10: %q", t)
		}
	case bool:
		if t {
			n.SetInt64(1)
		}
	default:
		return "", fmt.Errorf("int() argument must be a string or a real number, not '%s'", providers.TypeName(v))
	}
	s := n.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String(), nil
	}
	return b.String(), nil
}

// FetchFundingAndExec is sources.fetch_funding_and_exec: every Form D / D-A
// accession newer than cursor.sec_last_accession feeds BOTH detectors in one
// batch (one company_funded per accession, one executive_hired per NEW
// executive-titled related person). The CIK resolution side channel is
// patch["_resolved_cik"]. On a fetch failure it returns (nil, nil, err): the
// engine then leaves the cursor untouched.
func FetchFundingAndExec(ctx context.Context, d SourceDeps, w WatchView, wantFunding, wantExec, backfill bool) ([]DetectedEvent, map[string]any, error) {
	since := ""
	if sv := cursorGet(w, "sec_last_accession"); providers.Truthy(sv) {
		s, ok := sv.(string)
		if !ok {
			return nil, nil, fmt.Errorf("sec_last_accession is %s, not a string", providers.TypeName(sv))
		}
		since = s
	}
	filings, err := d.listFormD(ctx, w.Target, since)
	if err != nil {
		return nil, nil, err
	}
	resolved := w.ResolvedCIK
	if len(filings) > 0 && resolved == "" {
		resolved = filings[0].CIK
	}
	patch := map[string]any{}
	if resolved != "" && w.ResolvedCIK == "" {
		patch["_resolved_cik"] = resolved
	}
	knownExecs := cursorStrings(w, "sec_known_execs")
	knownSet := map[string]bool{}
	for _, k := range knownExecs {
		knownSet[k] = true
	}
	events := []DetectedEvent{}
	suppress := w.suppress(backfill)

	cik := resolved
	if cik == "" {
		cik = StableCompanyID(nil, w.Target)
	}
	for _, f := range filings {
		fcik := f.CIK
		if fcik == "" {
			fcik = cik
		}
		url := "https://www.sec.gov/cgi-bin/browse-edgar?action=getcompany&CIK=" + fcik + "&type=D"
		occurred := d.epochFromDate(f.FilingDate)
		if wantFunding && !suppress {
			title := "New SEC Form D filing"
			desc := "private_placement_form_d"
			if dv, ok := f.Fields["funding_stage_signal"]; ok {
				desc = providers.Str(dv)
			}
			if amount := f.Fields["funding_amount"]; providers.Truthy(amount) {
				s, aerr := formatAmount(amount)
				if aerr != nil {
					return nil, nil, aerr
				}
				title = "Raised $" + s + " (Form D)"
			}
			events = append(events, DetectedEvent{
				SignalType: "company_funded", NaturalEventID: fcik + ":" + f.Accession, Title: title,
				Description: desc, Source: "sec_edgar", SourceURL: url, Weight: 10, OccurredAt: occurred,
			})
		}
		if wantExec {
			for _, p := range f.RelatedPersons {
				name, titleStr := p["name"], p["title"]
				if name == "" || !IsExecutiveTitle(titleStr) {
					continue
				}
				tier := RoleTier(titleStr)
				nat := fcik + ":" + NormalizeExecName(name) + ":" + tier
				if knownSet[nat] {
					continue
				}
				knownSet[nat] = true
				knownExecs = append(knownExecs, nat)
				if !suppress {
					events = append(events, DetectedEvent{
						SignalType: "executive_hired", NaturalEventID: nat,
						Title:       "New " + strings.ReplaceAll(tier, "_", " ") + ": " + name,
						Description: name + " — " + titleStr + " (Form D related person)",
						Source:      "sec_edgar", SourceURL: url, Weight: 8, OccurredAt: occurred,
					})
				}
			}
		}
	}
	if len(filings) > 0 {
		patch["sec_last_accession"] = filings[0].Accession
	}
	if knownExecs == nil {
		knownExecs = []string{}
	}
	patch["sec_known_execs"] = knownExecs
	return events, patch, nil
}

// ── Website technographics → new_tech_adopted (source "website") ────────────

var webTechSoft = map[string]bool{"website_fetch_disabled": true, "No technologies detected": true, "robots_disallowed": true}

func (d SourceDeps) techProvider() (providers.Provider, error) {
	if d.TechStack != nil {
		return d.TechStack, nil
	}
	p, ok := providers.Get("tech_stack")
	if !ok {
		return nil, errors.New("tech_stack provider is not registered")
	}
	return p, nil
}

// FetchWebTech is sources.fetch_web_tech: homepage technographics diffed
// against cursor.known_web_tech. A disabled flag, an empty website, a robots
// refusal, a blocked URL or "no technologies" is a healthy no-op ([], {}); any
// other provider failure is (nil, nil, err) so the cursor does not advance.
func FetchWebTech(ctx context.Context, d SourceDeps, w WatchView, website string, backfill bool) ([]DetectedEvent, map[string]any, error) {
	if !d.Cfg.WebsiteTech || website == "" {
		return []DetectedEvent{}, map[string]any{}, nil
	}
	p, err := d.techProvider()
	if err != nil {
		return nil, nil, err
	}
	suppress := w.suppress(backfill)
	in := providers.Input{
		Lead:     map[string]any{"company": companyNameFor(w.Target), "website": website},
		Settings: map[string]any{"TECH_STACK_WEBSITE_FETCH_ENABLED": true},
	}
	out, err := p.Enrich(ctx, d.client(), func() (string, error) { return "", nil }, in)
	if err != nil {
		return nil, nil, err
	}
	if out.Error != "" {
		if webTechSoft[out.Error] || strings.HasPrefix(out.Error, "blocked_url") {
			return []DetectedEvent{}, map[string]any{}, nil
		}
		return nil, nil, errors.New(out.Error)
	}
	raw := "[]"
	if out.Extra != nil {
		if v, ok := out.Extra.Get("technographics"); ok && providers.Truthy(v) {
			raw = providers.Str(v)
		}
	}
	parsed, perr := providers.ParseJSON([]byte(raw))
	if perr != nil {
		return nil, nil, perr
	}
	techs, ok := parsed.([]any)
	if !ok {
		return nil, nil, fmt.Errorf("technographics is %s, not a list", providers.TypeName(parsed))
	}
	cik := w.stableID()
	known := cursorStrings(w, "known_web_tech")
	knownSet := map[string]bool{}
	for _, k := range known {
		knownSet[k] = true
	}
	events := []DetectedEvent{}
	for _, tv := range techs {
		t, ok := tv.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("technographics item is %s, not an object", providers.TypeName(tv))
		}
		rawName := ""
		if nv, ok := t["name"]; ok && nv != nil {
			rawName = providers.Str(nv)
		}
		tech := NormalizeTech(rawName)
		if tech == "" || knownSet[tech] {
			continue
		}
		knownSet[tech] = true
		known = append(known, tech)
		if suppress {
			continue
		}
		label := rawName
		if label == "" {
			label = tech
		}
		category := ""
		if cv, ok := t["category"]; ok {
			category = providers.Str(cv)
		}
		events = append(events, DetectedEvent{
			SignalType: "new_tech_adopted", NaturalEventID: cik + ":" + tech + ":website",
			Title: "Detected " + label + " on website", Description: category + ": " + label + " (source: website)",
			Source: "website", SourceURL: website, Weight: 6, OccurredAt: d.epochNow(),
		})
	}
	if known == nil {
		known = []string{}
	}
	return events, map[string]any{"known_web_tech": known}, nil
}

// ── RSS / news ──────────────────────────────────────────────────────────────

// FetchFeedSource is sources.fetch_feed: conditional-GET RSS/Atom diff keyed by
// GUID with the etag / last_modified / seen-id cursor keys (feed_etag,
// feed_last_modified, feed_seen_guids, feed_max_published).
func FetchFeedSource(ctx context.Context, d SourceDeps, w WatchView, backfill bool) ([]DetectedEvent, map[string]any, error) {
	suppress := w.suppress(backfill)
	fetch := d.FeedFetch
	if fetch == nil {
		fetch = func(ctx context.Context, d SourceDeps, u, e, l string) (FeedResult, error) {
			return FetchFeed(ctx, d, u, e, l)
		}
	}
	res, err := fetch(ctx, d, w.Target, cursorString(w, "feed_etag"), cursorString(w, "feed_last_modified"))
	if err != nil {
		return nil, nil, err
	}
	if res.Error != "" {
		return nil, nil, errors.New(res.Error)
	}
	validators := func(patch map[string]any) {
		if res.ETag != nil && *res.ETag != "" {
			patch["feed_etag"] = *res.ETag
		}
		if res.LastModified != nil && *res.LastModified != "" {
			patch["feed_last_modified"] = *res.LastModified
		}
	}
	if res.NotModified {
		patch := map[string]any{}
		validators(patch)
		return []DetectedEvent{}, patch, nil
	}
	seen := cursorStrings(w, "feed_seen_guids")
	seenSet := map[string]bool{}
	for _, g := range seen {
		seenSet[g] = true
	}
	maxPub := cursorFloat(w, "feed_max_published")
	newMax := maxPub
	events := []DetectedEvent{}
	for _, e := range res.Entries {
		if seenSet[e.GUID] {
			continue
		}
		seenSet[e.GUID] = true
		seen = append(seen, e.GUID)
		if e.PublishedEpoch > newMax {
			newMax = e.PublishedEpoch
		}
		if suppress {
			continue
		}
		title := e.Title
		if title == "" {
			title = "News mention"
		}
		link := e.Link
		if link == "" {
			link = w.Target
		}
		occurred := e.PublishedEpoch
		if occurred == 0 {
			occurred = d.epochNow()
		}
		events = append(events, DetectedEvent{
			SignalType: "news", NaturalEventID: e.GUID, Title: title, Description: e.Summary,
			Source: "rss", SourceURL: link, Weight: 5, OccurredAt: occurred,
		})
	}
	bound := 5 * d.feedMax()
	if bound < 200 {
		bound = 200
	}
	if len(seen) > bound {
		seen = seen[len(seen)-bound:]
	}
	if seen == nil {
		seen = []string{}
	}
	patch := map[string]any{"feed_seen_guids": seen, "feed_max_published": newMax}
	validators(patch)
	return events, patch, nil
}

// decodeFeedBody is httpx's resp.text charset step: UTF-8 unless the
// Content-Type names another charset the decoder knows.
func decodeFeedBody(body []byte, contentType string) []byte {
	if contentType == "" {
		return body
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return body
	}
	label := strings.ToLower(params["charset"])
	if label == "" || label == "utf-8" || label == "utf8" {
		return body
	}
	r, err := charset.NewReaderLabel(label, bytes.NewReader(body))
	if err != nil {
		return body
	}
	out, err := io.ReadAll(io.LimitReader(r, FeedMaxBody*4))
	if err != nil {
		return body
	}
	return out
}
