package declarative

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/kernels"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/template"
)

// Extractor selects records from a document. The production implementation
// is the Rust extraction kernel (internal/kernels); this package never
// parses HTML itself.
type Extractor interface {
	Extract(ctx context.Context, req kernels.ExtractRequest) (kernels.ExtractResult, error)
}

// ErrNoExtractor is returned when a scraper runs without an extraction kernel.
var ErrNoExtractor = errors.New("declarative scraper needs the extraction kernel (internal/kernels), which is not available in this build")

// Progress is reported after every fetched page.
type Progress struct {
	Pages   int    `json:"pages"`
	Records int    `json:"records"`
	URL     string `json:"url"`
}

// ScraperDeps are the host services a scraper run uses.
type ScraperDeps struct {
	Client    *egress.Client
	Extractor Extractor
	Secrets   SecretResolver
	// OnProgress is called after each page (optional).
	OnProgress func(Progress)
}

// RecordEvidence ties one record to the page and selectors that produced it.
type RecordEvidence struct {
	URL        string            `json:"url"`
	FetchedAt  time.Time         `json:"fetched_at"`
	Status     int               `json:"status"`
	Page       int               `json:"page"`
	Items      string            `json:"items,omitempty"`
	Fields     map[string]string `json:"fields"` // field -> selector
	BodySHA256 string            `json:"body_sha256"`
}

// Record is one scraped entity.
type Record struct {
	Fields   map[string]string `json:"fields"`
	Evidence RecordEvidence    `json:"evidence"`
}

// ScrapeResult is the outcome of a scraper run.
type ScrapeResult struct {
	Plugin  string   `json:"plugin"`
	Records []Record `json:"records"`
	Pages   int      `json:"pages"`
	// Stopped explains why pagination ended early (empty when it ran out of
	// pages naturally).
	Stopped string `json:"stopped,omitempty"`
}

// RunScraper renders the start URL, fetches pages through the egress client
// (robots.txt, rate limits, caps), extracts records with the kernel and
// follows pagination up to the page limit, deduplicating visited URLs and
// staying inside the network capability.
func RunScraper(ctx context.Context, p *manifest.Plugin, inputs map[string]any, deps ScraperDeps) (ScrapeResult, error) {
	res := ScrapeResult{Plugin: p.Name, Records: []Record{}}
	if p.Scrape == nil || p.Runtime != "declarative" || p.Kind != "scraper" {
		return res, errors.New("plugin is not a declarative scraper")
	}
	if deps.Client == nil {
		return res, errors.New("declarative: egress client is required")
	}
	if deps.Extractor == nil {
		return res, ErrNoExtractor
	}
	if err := p.ValidateInputs(inputs); err != nil {
		return res, err
	}
	sec, err := resolveDeclared(ctx, p, deps.Secrets)
	if err != nil {
		return res, err
	}
	s := p.Scrape
	tctx := pycompat.NewMap()
	_ = tctx.Set("input", pycompat.FromPlain(inputs))
	next := template.RenderString(s.Start, tctx, sec.env)

	maxPages := 1
	if s.Paginate != nil {
		maxPages = s.Paginate.MaxPages
		if maxPages <= 0 {
			maxPages = p.Limits.MaxPages
		}
	}
	if maxPages > p.Limits.MaxPages {
		maxPages = p.Limits.MaxPages
	}
	fields := make(map[string]string, len(s.Fields))
	for _, f := range s.Fields {
		fields[f.Name] = f.Selector
	}
	visited := map[string]bool{}
	for page := 1; page <= maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		u, err := url.Parse(next)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return res, fmt.Errorf("page %d: invalid URL %q", page, sec.Redact(next))
		}
		u.Fragment = ""
		key := u.String()
		if visited[key] {
			res.Stopped = "pagination loop: " + sec.Redact(key)
			break
		}
		visited[key] = true
		if err := p.Network().AllowsURL(u); err != nil {
			if page == 1 {
				return res, err
			}
			res.Stopped = "next page outside capabilities.network: " + sec.Redact(key)
			break
		}
		resp, err := deps.Client.Do(ctx, egress.Request{
			Method:       "GET",
			URL:          key,
			Robots:       true,
			RPS:          p.Limits.RequestsPerSecondPerDomain,
			MaxBodyBytes: p.Limits.MaxResponseBytes,
			Timeout:      time.Duration(p.Limits.TimeoutSeconds * float64(time.Second)),
			Allow:        p.Network().AllowsURL,
		})
		if err != nil {
			if page == 1 || ctx.Err() != nil {
				return res, fmt.Errorf("fetch %s: %s", sec.Redact(key), sec.Redact(err.Error()))
			}
			res.Stopped = "fetch failed: " + sec.Redact(err.Error())
			break
		}
		if resp.Status >= 400 {
			if page == 1 {
				return res, fmt.Errorf("fetch %s: http_%d", sec.Redact(key), resp.Status)
			}
			res.Stopped = fmt.Sprintf("http_%d on page %d", resp.Status, page)
			break
		}
		res.Pages = page
		base := resp.Evidence.URL
		out, err := deps.Extractor.Extract(ctx, kernels.ExtractRequest{
			Document: string(resp.Body), Format: s.Format, BaseURL: base, Items: s.Items, Fields: fields,
		})
		if err != nil {
			return res, fmt.Errorf("extract page %d: %w", page, err)
		}
		for _, rec := range out.Records {
			res.Records = append(res.Records, Record{Fields: rec, Evidence: RecordEvidence{
				URL: sec.Redact(base), FetchedAt: resp.Evidence.FetchedAt, Status: resp.Status, Page: page,
				Items: s.Items, Fields: fields, BodySHA256: resp.Evidence.SHA256,
			}})
		}
		if deps.OnProgress != nil {
			deps.OnProgress(Progress{Pages: res.Pages, Records: len(res.Records), URL: sec.Redact(base)})
		}
		if s.Paginate == nil || page == maxPages {
			break
		}
		nav, err := deps.Extractor.Extract(ctx, kernels.ExtractRequest{
			Document: string(resp.Body), Format: s.Format, BaseURL: base, Fields: map[string]string{"next": s.Paginate.Next},
		})
		if err != nil {
			return res, fmt.Errorf("extract next link on page %d: %w", page, err)
		}
		if len(nav.Records) == 0 || nav.Records[0]["next"] == "" {
			break
		}
		ref, err := url.Parse(nav.Records[0]["next"])
		if err != nil {
			res.Stopped = "invalid next link"
			break
		}
		baseURL, _ := url.Parse(base)
		next = baseURL.ResolveReference(ref).String()
	}
	return res, nil
}
