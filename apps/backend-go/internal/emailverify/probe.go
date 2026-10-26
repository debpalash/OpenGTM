package emailverify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// dnsTimeout is the MX lookup lifetime (LEADGEN_SMTP_TIMEOUT default 6s).
const dnsTimeout = 6 * time.Second

// ResolveMX is resolve_mx: the highest-priority MX host (lowest preference,
// first listed on a tie, trailing dot stripped) or "" when there is none or the
// lookup failed. Results, failures included, are remembered for an hour.
func ResolveMX(ctx context.Context, d Deps, domain string) string {
	if domain == "" {
		return ""
	}
	m, now := d.memo(), d.now()
	m.mu.Lock()
	host, ok := memoGet(m.mx, domain, now)
	m.mu.Unlock()
	if ok {
		return host
	}
	lctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	recs, err := d.Resolver.LookupMX(lctx, domain)
	cancel()
	if ctx.Err() != nil {
		return "" // a cancelled lookup says nothing about the domain
	}
	if err == nil && len(recs) > 0 {
		best := recs[0]
		for _, r := range recs[1:] {
			if r.Pref < best.Pref {
				best = r
			}
		}
		host = strings.TrimRight(best.Host, ".")
	}
	m.mu.Lock()
	memoPut(m.mx, domain, host, now)
	m.mu.Unlock()
	return host
}

// HasMX is has_mx.
func HasMX(ctx context.Context, d Deps, domain string) bool { return ResolveMX(ctx, d, domain) != "" }

// DomainProbe is DomainProbe: what one SMTP session learned about a domain.
// Results maps each candidate to true (accepted), false (hard reject) or nil
// (inconclusive: greylist, soft error, unreachable, catch-all).
type DomainProbe struct {
	Domain    string
	MXHost    string
	Reachable bool
	CatchAll  bool
	Results   map[string]*bool

	blocked error // egress refused the dial; ProbeSMTP reports it, the rest treat it as unknown
}

// Probe is the result of ProbeSMTP.
type Probe = DomainProbe

func boolPtr(b bool) *bool { return &b }

// ProbeDomain is probe_domain: ONE SMTP session to the domain's MX, a random
// RCPT first (catch-all calibration) then each candidate. It sends RCPT TO only
// and never any message data. A refused (private/rebinding) MX, an unreachable
// server or a timeout is "unknown", never a rejection. The error is non-nil
// only when ctx ends.
func ProbeDomain(ctx context.Context, d Deps, domain string, candidates []string) (DomainProbe, error) {
	p := DomainProbe{Domain: domain, Results: make(map[string]*bool, len(candidates))}
	for _, c := range candidates {
		p.Results[c] = nil
	}
	if d.SMTPDisabled {
		return p, nil
	}
	p.MXHost = ResolveMX(ctx, d, domain)
	if err := ctx.Err(); err != nil {
		return p, err
	}
	if p.MXHost == "" {
		return p, nil
	}
	p = probeHost(ctx, d, p, candidates)
	return p, ctx.Err()
}

// ProbeSMTP probes one address against one named host (no MX lookup): the same
// conversation as ProbeDomain for [email]. The error is non-nil when ctx ends
// or the egress guard refused the host (errors.Is(err, egress.ErrBlocked)).
func ProbeSMTP(ctx context.Context, d Deps, host, email string) (Probe, error) {
	domain := email
	if i := strings.LastIndex(email, "@"); i >= 0 {
		domain = email[i+1:]
	}
	p := DomainProbe{Domain: domain, MXHost: host, Results: map[string]*bool{email: nil}}
	if d.SMTPDisabled {
		return p, nil
	}
	p = probeHost(ctx, d, p, []string{email})
	if err := ctx.Err(); err != nil {
		return p, err
	}
	return p, p.blocked
}

func probeHost(ctx context.Context, d Deps, p DomainProbe, candidates []string) DomainProbe {
	if d.NewSession == nil {
		return p
	}
	ctx, cancel := context.WithTimeout(ctx, d.Timeout())
	defer cancel()
	sess := d.NewSession()
	defer func() {
		if err := sess.Quit(); err != nil {
			sess.Close()
		}
	}()
	if err := sess.Connect(ctx, p.MXHost, d.port()); err != nil {
		if errors.Is(err, egress.ErrBlocked) {
			p.blocked = fmt.Errorf("smtp probe of %q refused: %w", p.MXHost, err)
		}
		return p
	}
	if err := sess.EhloOrHelo(d.helo()); err != nil {
		return p
	}
	if err := sess.Helo(d.helo()); err != nil {
		return p
	}
	code, err := sess.Mail(d.mailFrom())
	if err != nil || code >= 400 {
		return p // MAIL FROM refused: cannot probe, leave unknown
	}
	p.Reachable = true

	m := d.memo()
	m.mu.Lock()
	catchAll, cached := memoGet(m.catchAll, p.Domain, d.now())
	m.mu.Unlock()
	if cached {
		p.CatchAll = catchAll
	} else {
		rcode, err := sess.Rcpt(d.randLocal() + "@" + p.Domain)
		if err != nil {
			return p
		}
		p.CatchAll = rcode < 400 // accepted an address that cannot exist
		m.mu.Lock()
		memoPut(m.catchAll, p.Domain, p.CatchAll, d.now())
		m.mu.Unlock()
	}
	if p.CatchAll {
		return p // individual checks are unreliable; results stay unknown
	}
	for _, email := range candidates {
		rcode, err := sess.Rcpt(email)
		switch {
		case errors.Is(err, ErrDisconnected):
			return p
		case errors.Is(err, ErrSMTP):
			p.Results[email] = nil
			continue
		case err != nil:
			return p // transport failure: abort, keep what we have
		}
		switch {
		case rcode < 400:
			p.Results[email] = boolPtr(true)
		case rcode == 550 || rcode == 551 || rcode == 553 || rcode == 554:
			p.Results[email] = boolPtr(false)
		default:
			p.Results[email] = nil // soft/greylist/other 5xx: unknown
		}
	}
	return p
}

// EmailCheck is email_verify.VerifyResult (the older single-address verdict
// used by the pattern path).
type EmailCheck struct {
	Email          string
	Confidence     string // smtp_verified | pattern | generic | "" (none)
	Deliverable    *bool
	CatchAll       bool
	Reachable      bool
	Classification *Classification
}

// VerifyEmail is email_verify.verify_email: classify then SMTP-probe one address.
func VerifyEmail(ctx context.Context, d Deps, email string) (EmailCheck, error) {
	cls := Classify(email)
	if !cls.ValidSyntax {
		return EmailCheck{Email: email, Classification: &cls}, nil
	}
	p, err := ProbeDomain(ctx, d, cls.Domain, []string{cls.Email})
	if err != nil {
		return EmailCheck{}, err
	}
	deliverable := p.Results[cls.Email]
	var conf string
	switch {
	case deliverable != nil && *deliverable:
		conf = "smtp_verified"
	case p.CatchAll || deliverable == nil:
		conf = "pattern"
		if cls.IsRole {
			conf = "generic"
		}
	}
	out := EmailCheck{Confidence: conf, Deliverable: deliverable, CatchAll: p.CatchAll, Reachable: p.Reachable, Classification: &cls}
	if conf != "" {
		out.Email = email
	}
	return out, nil
}

// PickBestEmail is pick_best_email: probe the pattern candidates for ONE domain
// in a single session and return the first deliverable one.
func PickBestEmail(ctx context.Context, d Deps, candidates []string) (EmailCheck, error) {
	var cs []string
	for _, c := range candidates {
		if c != "" && strings.Contains(c, "@") {
			cs = append(cs, c)
		}
	}
	if len(cs) == 0 {
		return EmailCheck{}, nil
	}
	parts := strings.Split(cs[0], "@")
	domain := parts[1]
	p, err := ProbeDomain(ctx, d, domain, cs)
	if err != nil {
		return EmailCheck{}, err
	}
	for _, c := range cs {
		if r := p.Results[c]; r != nil && *r {
			cls := Classify(c)
			return EmailCheck{Email: c, Confidence: "smtp_verified", Deliverable: boolPtr(true), Reachable: p.Reachable, Classification: &cls}, nil
		}
	}
	allUnknown := true
	for _, r := range p.Results {
		if r != nil {
			allUnknown = false
		}
	}
	if p.CatchAll || !p.Reachable || allUnknown {
		if HasMX(ctx, d, domain) {
			cls := Classify(cs[0])
			return EmailCheck{Email: cs[0], Confidence: "pattern", CatchAll: p.CatchAll, Reachable: p.Reachable, Classification: &cls}, nil
		}
		return EmailCheck{CatchAll: p.CatchAll, Reachable: p.Reachable}, nil
	}
	return EmailCheck{Deliverable: boolPtr(false), Reachable: p.Reachable}, nil
}
