package ambitionbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourceingest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	// Gateway is the fixed AmbitionBox service gateway. Production clients never
	// take a base from user input; tests point Base at a local simulator.
	Gateway = "https://www.ambitionbox.com/servicegateway-ambitionbox"
	// MaxBody bounds every response read (Python reads it whole).
	MaxBody = 4 << 20
	// Timeout is the per-attempt total deadline (aiohttp ClientTimeout total=15).
	Timeout  = 15 * time.Second
	attempts = 3
	pageSize = 20
)

var headers = [][2]string{
	{"appid", "125"},
	{"systemid", "local"},
	{"content-type", "application/json"},
	{"accept", "application/json"},
	{"user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"},
	{"origin", "https://www.ambitionbox.com"},
	{"referer", "https://www.ambitionbox.com/list-of-companies"},
}

// Client talks to the AmbitionBox gateway. HTTP should be the egress SSRF-safe
// client; redirects are refused on a copy of it. Base empty = Gateway. Sleep
// nil = a context-aware timer.
type Client struct {
	HTTP  *http.Client
	Base  string
	Sleep func(context.Context, time.Duration) error

	// fetch replaces the page fetch in collection tests.
	fetch func(context.Context, PageParams) (sourceingest.Page, error)
}

// New returns a client for the real gateway over c (nil = a plain client).
func New(c *http.Client) *Client { return &Client{HTTP: c, Base: Gateway} }

// UpstreamError is a failed gateway call. Status is the last HTTP status seen
// (0 for a network failure or timeout). Routes map it to 502 (D4).
type UpstreamError struct {
	Status int
	Msg    string
}

func (e *UpstreamError) Error() string { return e.Msg }

// ErrFilterUnsupported is Python's NotImplementedError for the filters the
// gateway silently ignores. Field is "location" or "company_type".
type ErrFilterUnsupported struct{ Field string }

func (e *ErrFilterUnsupported) Error() string {
	return "AmbitionBox's gateway ignores " + e.Field + " filters -- refusing to return silently unfiltered results"
}

// PageError wraps a failed page of a collection:
// "AmbitionBox company search failed on page N: <err>".
type PageError struct {
	Page int
	Err  error
}

func (e *PageError) Error() string {
	return "AmbitionBox company search failed on page " + strconv.Itoa(e.Page) + ": " + e.Err.Error()
}
func (e *PageError) Unwrap() error { return e.Err }

func (c *Client) base() string {
	if c.Base == "" {
		return Gateway
	}
	return c.Base
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) httpClient() *http.Client {
	var cc http.Client
	if c.HTTP != nil {
		cc = *c.HTTP
	}
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cc
}

// transportText renders a transport failure like str(exc) (timeouts have no
// text in Python; "timeout" is clearer and carries no URL).
func transportText(err error) string {
	if providers.IsTimeout(err) {
		return "timeout"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

type attemptResult struct {
	doc   any
	last  *UpstreamError // retryable failure
	fatal error          // do not retry
}

func (c *Client) once(ctx context.Context, hc *http.Client, method, path string, body *Obj) attemptResult {
	cctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		b, _ := body.MarshalJSON()
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(cctx, method, c.base()+"/"+path, rdr)
	if err != nil {
		return attemptResult{fatal: &UpstreamError{Msg: "AmbitionBox request could not be built"}}
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return attemptResult{fatal: ctx.Err()}
		}
		return attemptResult{last: &UpstreamError{Msg: transportText(err)}}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return attemptResult{fatal: ctx.Err()}
		}
		return attemptResult{last: &UpstreamError{Status: resp.StatusCode, Msg: transportText(err)}}
	}
	if resp.StatusCode == http.StatusOK {
		if len(raw) > MaxBody {
			return attemptResult{fatal: &UpstreamError{Status: 200, Msg: fmt.Sprintf("AmbitionBox response exceeds %d bytes", MaxBody)}}
		}
		doc, errs := wbcrud.DecodeBody(raw)
		if errs != nil {
			return attemptResult{fatal: &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: response is not valid JSON"}}
		}
		return attemptResult{doc: doc}
	}
	msg := "AmbitionBox API " + strconv.Itoa(resp.StatusCode) + ": " +
		providers.Trunc200(strings.ToValidUTF8(string(raw), "�"))
	e := &UpstreamError{Status: resp.StatusCode, Msg: msg}
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return attemptResult{fatal: e}
	}
	return attemptResult{last: e}
}

// request is _request: three attempts, retry only 429/5xx/network/timeout with
// 0.5 s * 2^n backoff, 4xx fails at once. A cancelled context stops everything.
func (c *Client) request(ctx context.Context, method, path string, body *Obj) (any, error) {
	hc := c.httpClient()
	var last *UpstreamError
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r := c.once(ctx, hc, method, path, body)
		if r.fatal != nil {
			return nil, r.fatal
		}
		if r.last == nil {
			return r.doc, nil
		}
		last = r.last
		if attempt < attempts-1 {
			if err := c.sleep(ctx, 500*time.Millisecond<<attempt); err != nil {
				return nil, err
			}
		}
	}
	return nil, &UpstreamError{Status: last.Status,
		Msg: "AmbitionBox request failed after " + strconv.Itoa(attempts) + " attempts: " + last.Msg}
}

func asObj(doc any) (*Obj, error) {
	if o, ok := doc.(*Obj); ok {
		return o, nil
	}
	return nil, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected response shape"}
}

// PageParams are the inputs of fetch_company_page. Zero Page = 1, empty SortBy
// = "popular".
type PageParams struct {
	Page        int
	SortBy      string
	Industry    []string
	Location    []string
	CompanyType []string
	Rating      string
}

// SearchParams are the inputs of search_companies. Limit trims client side
// (0 = no trim; the gateway always returns 20).
type SearchParams struct {
	Page        int
	Limit       int
	SortBy      string
	Industry    []string
	Location    []string
	CompanyType []string
	Rating      string
}

func (p PageParams) norm() PageParams {
	if p.Page == 0 {
		p.Page = 1
	}
	if p.SortBy == "" {
		p.SortBy = "popular"
	}
	return p
}

// FetchCompanyPage fetches one source page with pagination metadata; every
// Record.Data is the ordered company dict. Unsupported filters fail with
// *ErrFilterUnsupported before any request.
func (c *Client) FetchCompanyPage(ctx context.Context, p PageParams) (sourceingest.Page, error) {
	p = p.norm()
	body := wbcrud.NewObj()
	body.Set("isFilterApplied", true)
	body.Set("page", strconv.Itoa(p.Page))
	body.Set("sortBy", p.SortBy)
	body.Set("limit", pageSize)
	if len(p.Industry) > 0 {
		slugs := make([]any, len(p.Industry))
		for i, s := range p.Industry {
			slugs[i] = industrySlug(s)
		}
		body.Set("industries", slugs)
	}
	if p.Rating != "" {
		body.Set("ratings", p.Rating)
	}
	if len(p.Location) > 0 {
		return sourceingest.Page{}, &ErrFilterUnsupported{Field: "location"}
	}
	if len(p.CompanyType) > 0 {
		return sourceingest.Page{}, &ErrFilterUnsupported{Field: "company_type"}
	}
	doc, err := c.request(ctx, http.MethodPost, "company-services/v0/listing/dream/companies/search", body)
	if err != nil {
		return sourceingest.Page{}, err
	}
	data, err := asObj(doc)
	if err != nil {
		return sourceingest.Page{}, err
	}
	var cards []any
	if v := get(data, "cards"); truthy(v) {
		var ok bool
		if cards, ok = v.([]any); !ok {
			return sourceingest.Page{}, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected response shape"}
		}
	}
	now := time.Now().UTC()
	recs := make([]sourceingest.Record, 0, len(cards))
	for offset, cv := range cards {
		card, ok := cv.(*Obj)
		if !ok {
			return sourceingest.Page{}, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected card shape"}
		}
		co, err := CompanyFromAPI(card)
		if err != nil {
			return sourceingest.Page{}, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected card: " + err.Error()}
		}
		d := co.ToDict()
		profile, _ := d.Get("profile_url")
		prof := profile.(string)
		rec := sourceingest.Record{Provider: "ambitionbox", RecordID: recordID(co), Data: d,
			Rank: (p.Page-1)*pageSize + offset, FetchedAt: now}
		if prof != "" {
			rec.SourceURL = &prof
		}
		recs = append(recs, rec)
	}
	stats := wbcrud.NewObj()
	if v := get(data, "stats"); truthy(v) {
		var ok bool
		if stats, ok = v.(*Obj); !ok {
			return sourceingest.Page{}, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected stats shape"}
		}
	}
	total, err := optInt(get(stats, "totalCards"))
	if err != nil {
		return sourceingest.Page{}, err
	}
	pages, err := optInt(get(stats, "totalPages"))
	if err != nil {
		return sourceingest.Page{}, err
	}
	return sourceingest.NewPage("ambitionbox", recs, p.Page, pageSize, total, pages), nil
}

// recordID is str(company_id), or "name:"+casefolded name when the id is
// None/""/0.
func recordID(co ABCompany) string {
	switch v := co.CompanyID.(type) {
	case nil:
	case string:
		if v != "" {
			return v
		}
	default:
		if truthy(v) {
			return pyStr(v)
		}
	}
	return "name:" + casefold(providers.PyStrip(co.Name))
}

// optInt is `int(x) if x is not None else None` for stats values.
func optInt(v any) (*int, error) {
	var n int
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return nil, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected stats value"}
		}
		n = i
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return nil, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected stats value"}
		}
		n = int(f)
	default:
		return nil, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected stats value"}
	}
	return &n, nil
}

// SearchCompanies is search_companies: {companies, total, page, source_total,
// total_pages, has_more}. companies are ordered company dicts.
func (c *Client) SearchCompanies(ctx context.Context, p SearchParams) (*Obj, error) {
	pp := PageParams{Page: p.Page, SortBy: p.SortBy, Industry: p.Industry, Location: p.Location,
		CompanyType: p.CompanyType, Rating: p.Rating}.norm()
	pg, err := c.FetchCompanyPage(ctx, pp)
	if err != nil {
		return nil, err
	}
	recs := pg.Records
	if p.Limit > 0 && len(recs) > p.Limit {
		recs = recs[:p.Limit]
	}
	companies := make([]any, len(recs))
	for i, r := range recs {
		companies[i] = r.Data
	}
	o := wbcrud.NewObj()
	o.Set("companies", companies)
	o.Set("total", len(companies))
	o.Set("page", pp.Page)
	o.Set("source_total", intPtr(pg.SourceTotal))
	o.Set("total_pages", intPtr(pg.TotalPages))
	o.Set("has_more", pg.HasMore)
	return o, nil
}

func intPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// GetCompanyJobs is get_company_jobs: {jobs, total, page, total_pages}.
func (c *Client) GetCompanyJobs(ctx context.Context, companyID, page int) (*Obj, error) {
	doc, err := c.request(ctx, http.MethodGet, "jobs-services/v0/jobs/company/"+strconv.Itoa(companyID)+"?page="+strconv.Itoa(page), nil)
	if err != nil {
		return nil, err
	}
	data, err := asObj(doc)
	if err != nil {
		return nil, err
	}
	list, ok := orDefault(data, "jobs", []any{}).([]any)
	if !ok {
		return nil, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected response shape"}
	}
	jobs := make([]any, 0, len(list))
	for _, jv := range list {
		j, ok := jv.(*Obj)
		if !ok {
			return nil, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected job shape"}
		}
		job, err := JobFromAPI(j)
		if err != nil {
			return nil, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected job: " + err.Error()}
		}
		jobs = append(jobs, job.ToDict())
	}
	pag, ok := orDefault(data, "pagination", wbcrud.NewObj()).(*Obj)
	if !ok {
		return nil, &UpstreamError{Status: 200, Msg: "AmbitionBox API 200: unexpected response shape"}
	}
	o := wbcrud.NewObj()
	o.Set("jobs", jobs)
	o.Set("total", orDefault(pag, "count", int64(0)))
	o.Set("page", orDefault(pag, "currentPage", int64(1)))
	o.Set("total_pages", orDefault(pag, "totalPages", int64(1)))
	return o, nil
}

// GetCompanyDetail returns the gateway's sectionalDetails document unchanged
// (an ordered tree: *wbcrud.Obj, []any, json.Number, ...).
func (c *Client) GetCompanyDetail(ctx context.Context, companyID int) (any, error) {
	return c.request(ctx, http.MethodGet, "company-services/v0/company/"+strconv.Itoa(companyID)+"/sectionalDetails", nil)
}

// GetSimilarCompanies returns the similar-companies document unchanged.
func (c *Client) GetSimilarCompanies(ctx context.Context, companyID int) (any, error) {
	return c.request(ctx, http.MethodGet, "insights-services/v0/company/"+strconv.Itoa(companyID)+"/similar-companies-v3", nil)
}

// Collection is ConnectorCollection.
type Collection struct {
	Provider        string
	Records         []sourceingest.Record
	RequestedCount  int
	PagesFetched    int
	NextPage        int
	SourceTotal     *int
	TargetMet       bool
	Exhausted       bool
	MaxPagesReached bool
	Warnings        []string
}

// Partial is `not target_met`.
func (c Collection) Partial() bool { return !c.TargetMet }

// Status is "complete" or "partial".
func (c Collection) Status() string {
	if c.TargetMet {
		return "complete"
	}
	return "partial"
}

// Summary is ConnectorCollection.summary (key order included).
func (c Collection) Summary(includeRecords bool) *Obj {
	o := wbcrud.NewObj()
	o.Set("provider", c.Provider)
	o.Set("requested_count", c.RequestedCount)
	o.Set("returned_count", len(c.Records))
	o.Set("pages_fetched", c.PagesFetched)
	o.Set("next_page", c.NextPage)
	o.Set("source_total", intPtr(c.SourceTotal))
	o.Set("target_met", c.TargetMet)
	o.Set("exhausted", c.Exhausted)
	o.Set("partial", c.Partial())
	o.Set("status", c.Status())
	o.Set("max_pages_reached", c.MaxPagesReached)
	warnings := make([]any, len(c.Warnings))
	for i, w := range c.Warnings {
		warnings[i] = w
	}
	o.Set("warnings", warnings)
	if includeRecords {
		recs := make([]any, len(c.Records))
		for i, r := range c.Records {
			recs[i] = r.Data
		}
		o.Set("records", recs)
	}
	return o
}

// CollectParams are the inputs of collect_companies. Requested is floored at
// 1, MaxPages clamped to 1..25, StartPage floored at 1.
type CollectParams struct {
	Requested int
	MaxPages  int
	StartPage int
	Industry  []string
	Location  []string
	SortBy    string
	Rating    string
}

// CollectCompanies collects until the target is met or the source is
// exhausted (or maxPages is spent).
func (c *Client) CollectCompanies(ctx context.Context, requested, maxPages int, industry, location []string) (Collection, error) {
	return c.CollectCompaniesWith(ctx, CollectParams{Requested: requested, MaxPages: maxPages, Industry: industry, Location: location})
}

// CollectCompaniesWith is collect_companies with every parameter.
func (c *Client) CollectCompaniesWith(ctx context.Context, p CollectParams) (Collection, error) {
	requested := max(1, p.Requested)
	maxPages := max(1, min(p.MaxPages, 25))
	page := max(1, p.StartPage)
	sortBy := p.SortBy
	if sortBy == "" {
		sortBy = "popular"
	}
	fetch := c.fetch
	if fetch == nil {
		fetch = c.FetchCompanyPage
	}
	var (
		recs      []sourceingest.Record
		seen      = map[string]bool{}
		fetched   int
		total     *int
		exhausted bool
	)
	for fetched < maxPages && len(recs) < requested {
		res, err := fetch(ctx, PageParams{Page: page, SortBy: sortBy, Industry: p.Industry, Location: p.Location, Rating: p.Rating})
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return Collection{}, cerr
			}
			return Collection{}, &PageError{Page: page, Err: err}
		}
		fetched++
		total = res.SourceTotal
		for _, r := range res.Records {
			if seen[r.RecordID] {
				continue
			}
			seen[r.RecordID] = true
			recs = append(recs, r)
			if len(recs) >= requested {
				break
			}
		}
		page++
		if !res.HasMore {
			exhausted = true
			break
		}
	}
	met := len(recs) >= requested
	maxReached := !met && !exhausted && fetched >= maxPages
	col := Collection{Provider: "ambitionbox", Records: recs, RequestedCount: requested, PagesFetched: fetched,
		NextPage: page, SourceTotal: total, TargetMet: met, Exhausted: exhausted, MaxPagesReached: maxReached, Warnings: []string{}}
	if maxReached {
		col.Warnings = []string{"maximum page budget reached before requested target"}
	}
	return col, nil
}

var foldExtra = strings.NewReplacer("ß", "ss", "ẞ", "ss", "ς", "σ", "ſ", "s", "ﬀ", "ff", "ﬁ", "fi", "ﬂ", "fl", "ﬃ", "ffi", "ﬄ", "ffl", "ﬅ", "st", "ﬆ", "st", "İ", "i̇")

// casefold is str.casefold for the scripts that differ from lower(): sharp s,
// final sigma, long s and the Latin ligatures.
func casefold(s string) string { return foldExtra.Replace(strings.ToLower(s)) }
