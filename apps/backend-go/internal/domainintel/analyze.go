// Package domainintel is the free, keyless domain intelligence of enrichment/domain_intel.py: Cloudflare DoH
// records, RDAP registration data and crt.sh subdomains, folded into a 0-100 legitimacy score.
//
// Every request is made through the caller's *http.Client, which production builds with the egress guard
// (egress.NewClientForEnv / egress.NewDialContext: private, loopback, link-local and metadata addresses are
// refused at dial time, after a single resolution). The request hosts are fixed (cloudflare-dns.com, rdap.org,
// crt.sh); the user's domain, validated first, only appears in query values and the RDAP/crt.sh target.
package domainintel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	defaultDeadline = 15 * time.Second
	maxConcurrent   = 5
	maxRedirects    = 5
	dohTimeout      = 5 * time.Second
	rdapTimeout     = 10 * time.Second
	crtTimeout      = 12 * time.Second
)

// ErrInvalidDomain is returned for input that is not an already-cleaned domain (see Clean).
var ErrInvalidDomain = errors.New("domainintel: invalid domain")

type redirectPolicy int

const (
	sameHostOnly redirectPolicy = iota // DoH and crt.sh: a redirect must stay on the same host
	anyHTTPS                           // RDAP: rdap.org redirects to the registry's RDAP server
)

type analyzer struct {
	c        *http.Client
	now      func() time.Time
	sleep    func(context.Context, time.Duration)
	deadline time.Duration
	sem      chan struct{}
}

func newAnalyzer(c *http.Client) *analyzer {
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse } // followed by get, under policy
	return &analyzer{
		c:        &cc,
		now:      time.Now,
		deadline: defaultDeadline,
		sleep: func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
			}
		},
	}
}

// NewClient is the production HTTP client: egress-guarded, no proxy, redirects left to this package.
// allowHosts is honoured only when appEnv is test or bench; production passes none.
func NewClient(appEnv string, allowHosts []string) *http.Client {
	return egress.NewClientForEnv(appEnv, allowHosts)
}

// Analyze is analyze_domain for a cleaned domain (see Clean). Upstream failures and timeouts yield empty sections
// like Python; an error is returned only for an invalid domain, a cancelled ctx, or a non-object crt.sh row (a
// Python bug that raises there; Go returns the error instead of crashing). The whole analysis is bounded by 15 s.
func Analyze(ctx context.Context, c *http.Client, domain string) (*Obj, error) {
	return newAnalyzer(c).analyze(ctx, domain)
}

func (a *analyzer) get(ctx context.Context, rawURL string, hdr map[string]string, timeout time.Duration, limit int64, pol redirectPolicy) (int, []byte, error) {
	if a.sem == nil {
		return 0, nil, errors.New("domainintel: analyzer not initialised")
	}
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	first, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, err
	}
	cur := first
	for hop := 0; hop <= maxRedirects; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur.String(), nil)
		if err != nil {
			return 0, nil, err
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := a.c.Do(req)
		if err != nil {
			return 0, nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if loc != "" {
				next, err := cur.Parse(loc)
				if err != nil || next.Scheme != "https" || next.Host == "" || (pol == sameHostOnly && next.Host != first.Host) {
					return resp.StatusCode, nil, errors.New("domainintel: redirect refused")
				}
				cur = next
				continue
			}
			return resp.StatusCode, nil, nil
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		_ = resp.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		if int64(len(body)) > limit {
			return resp.StatusCode, nil, errors.New("domainintel: response too large")
		}
		return resp.StatusCode, body, nil
	}
	return 0, nil, errors.New("domainintel: too many redirects")
}

func decodeJSON(b []byte) (any, bool) {
	doc, errs := wbcrud.DecodeBody(b)
	return doc, len(errs) == 0
}

func (a *analyzer) analyze(parent context.Context, domain string) (*Obj, error) {
	if !validDomain(domain) {
		return nil, ErrInvalidDomain
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	a.sem = make(chan struct{}, maxConcurrent)
	ctx, cancel := context.WithTimeout(parent, a.deadline)
	defer cancel()

	var (
		wg                    sync.WaitGroup
		rd                    rdapInfo
		crt                   crtInfo
		crtErr                error
		aR, mxR, nsR, txtR, d []string
	)
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() { rd = a.rdap(ctx, domain) })
	run(func() { aR = a.query(ctx, domain, "A") })
	run(func() { mxR = a.query(ctx, domain, "MX") })
	run(func() { nsR = a.query(ctx, domain, "NS") })
	run(func() { txtR = a.query(ctx, domain, "TXT") })
	run(func() { d = a.query(ctx, "_dmarc."+domain, "TXT") })
	run(func() { crt, crtErr = a.crt(ctx, domain) })
	wg.Wait()
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if crtErr != nil {
		return nil, crtErr
	}
	dns := buildDNS(aR, mxR, nsR, txtR, d)
	legit := score(scoreInput{ageDays: rd.AgeDays, hasSPF: dns.HasSPF, hasDMARC: dns.HasDMARC, emailProvider: dns.Email,
		mx: len(dns.MX), hosting: dns.Hosting, subdomains: crt.Count})

	summary := NewObj()
	summary.Set("registrar", rd.Registrar)
	summary.Set("age_days", rd.AgeDays)
	summary.Set("hosting", dns.Hosting)
	summary.Set("email_service", dns.Email)
	summary.Set("has_email_auth", dns.HasSPF && dns.HasDMARC)
	summary.Set("subdomains", crt.Count)
	summary.Set("interesting_subdomains", strs(crt.Interesting))

	out := NewObj()
	out.Set("domain", domain)
	out.Set("rdap", rd.obj())
	out.Set("dns", dns.obj())
	out.Set("cert_transparency", crt.obj())
	out.Set("legitimacy_score", legit)
	out.Set("summary", summary)
	return out, nil
}
