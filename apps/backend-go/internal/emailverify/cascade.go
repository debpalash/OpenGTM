package emailverify

import (
	"context"
	"errors"
	"strings"
)

// The four normalised statuses every verifier maps to.
const (
	StatusValid    = "valid"
	StatusInvalid  = "invalid"
	StatusCatchAll = "catch_all"
	StatusUnknown  = "unknown"
)

var confidenceByStatus = map[string]float64{StatusValid: 0.95, StatusInvalid: 0.9, StatusCatchAll: 0.5, StatusUnknown: 0}

// Attempt is one verifier's entry in Result.Attempts.
type Attempt struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
}

// Result is VerifyResult of email_verify_cascade.py, field order as Python's dataclass.
type Result struct {
	Email      string    `json:"email"`
	Status     string    `json:"status"` // valid | invalid | catch_all | unknown
	Source     string    `json:"source"` // which verifier produced it
	Confidence float64   `json:"confidence"`
	Detail     string    `json:"detail"`
	Attempts   []Attempt `json:"attempts"`
}

// IsDefinitive is valid, invalid or catch_all: the cascade stops there.
func (r Result) IsDefinitive() bool {
	return r.Status == StatusValid || r.Status == StatusInvalid || r.Status == StatusCatchAll
}

// Deliverable counts catch_all as "risky deliverable", usable for cold outreach.
func (r Result) Deliverable() bool { return r.Status == StatusValid || r.Status == StatusCatchAll }

// StatusFromVendor maps a vendor status string to the 4-status contract
// (_status_from_vendor; matching is on the lower-cased raw string, not stripped).
func StatusFromVendor(raw string) string {
	switch strings.ToLower(raw) {
	case "valid", "deliverable", "ok", "safe":
		return StatusValid
	case "invalid", "undeliverable", "bad", "rejected", "disposable":
		return StatusInvalid
	case "catch_all", "catchall", "accept_all", "accept-all", "unknown_catch_all":
		return StatusCatchAll
	}
	return StatusUnknown
}

// Verifier is one tier of the cascade. Available may fail (the verifier is then
// skipped). Verify returns an error for an infrastructure failure the cascade
// records as an "error" attempt and moves past; if the error implements
// Kind() string, that is the attempt detail (Python: the exception type name).
type Verifier interface {
	Name() string
	Available(ctx context.Context) (bool, error)
	Verify(ctx context.Context, email string) (Result, error)
}

// binder lets a registered verifier pick up the live Deps at cascade time.
type binder interface{ bind(Deps) Verifier }

// ---- registry ---------------------------------------------------------------

// Registry is the ordered verifier list (_VERIFIERS), SMTP first.
type Registry struct{ vs []Verifier }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register appends v, or prepends it when front is set.
func (r *Registry) Register(v Verifier, front bool) {
	if front {
		r.vs = append([]Verifier{v}, r.vs...)
		return
	}
	r.vs = append(r.vs, v)
}

// All is every registered verifier in order.
func (r *Registry) All() []Verifier { return append([]Verifier(nil), r.vs...) }

// Available is available_verifiers: registered verifiers that report available
// (one that errors is skipped).
func (r *Registry) Available(ctx context.Context) []Verifier {
	out := []Verifier{}
	for _, v := range r.vs {
		if ok, err := v.Available(ctx); err == nil && ok {
			out = append(out, v)
		}
	}
	return out
}

// ---- SMTP verifier (layer 0) ------------------------------------------------

type smtpVerifier struct {
	d     Deps
	bound bool
}

// SMTPVerifier is SmtpVerifier bound to d: the free port-25 probe.
func SMTPVerifier(d Deps) Verifier { return &smtpVerifier{d: d, bound: true} }

func (s *smtpVerifier) bind(d Deps) Verifier {
	if s.bound {
		return s
	}
	return &smtpVerifier{d: d, bound: true}
}

func (*smtpVerifier) Name() string { return "smtp" }

// Available is false only when probing is disabled (port 25 blocked in cloud).
func (s *smtpVerifier) Available(context.Context) (bool, error) { return !s.d.SMTPDisabled, nil }

func (s *smtpVerifier) Verify(ctx context.Context, email string) (Result, error) {
	domain := ""
	if i := strings.LastIndex(email, "@"); i >= 0 {
		domain = email[i+1:]
	}
	mk := func(status, detail string) Result {
		return Result{Email: email, Status: status, Source: "smtp", Confidence: confidenceByStatus[status], Detail: detail, Attempts: []Attempt{}}
	}
	if domain == "" {
		return mk(StatusInvalid, "no_domain"), nil
	}
	p, err := ProbeDomain(ctx, s.d, domain, []string{email})
	if err != nil {
		return Result{}, err
	}
	switch res := p.Results[email]; {
	case !p.Reachable:
		return mk(StatusUnknown, "unreachable"), nil
	case p.CatchAll:
		return mk(StatusCatchAll, "catch_all"), nil
	case res != nil && *res:
		return mk(StatusValid, "rcpt_accepted"), nil
	case res != nil:
		return mk(StatusInvalid, "rcpt_rejected"), nil
	}
	return mk(StatusUnknown, "soft"), nil
}

// ---- cache ------------------------------------------------------------------

// CacheEntry is the cached verdict for (email, verifier): the jsonb object
// {status, source, detail, confidence}.
type CacheEntry struct {
	Status     string  `json:"status"`
	Source     string  `json:"source"`
	Detail     string  `json:"detail"`
	Confidence float64 `json:"confidence"`
}

// VerifyCache is the per-workspace email verification cache. Implementations
// key on enrichcache.EmailKey and must never serve one workspace's entry to
// another. Errors are treated as a miss / a lost write, like Python.
type VerifyCache interface {
	Get(ctx context.Context, workspaceID, email, verifier string) (CacheEntry, bool, error)
	Put(ctx context.Context, workspaceID, email, verifier string, e CacheEntry) error
}

var errorDetails = map[string]bool{"error": true, "timeout": true, "breaker_open": true, "http_error": true, "unconfigured": true}

// isTransient is _is_transient: an UNKNOWN that is an infrastructure failure.
func isTransient(r Result) bool { return r.Status == StatusUnknown && errorDetails[r.Detail] }

// ---- cascade ----------------------------------------------------------------

// Cascade is verify_email of email_verify_cascade.py. The chain is the
// workspace's Reacher (when enabled, configured and its breaker is closed)
// followed by the registered verifiers that report available, SMTP first. The
// first definitive result wins; unknown or an error moves on; otherwise the last
// result seen is returned. The per-email cache is engaged only when Reacher is
// in the chain (a Reacher-off install writes nothing), exactly like Python.
// The error is non-nil only when ctx ends.
func Cascade(ctx context.Context, d Deps, workspaceID, email string) (Result, error) {
	if email == "" || !strings.Contains(email, "@") {
		return badSyntax(email), nil
	}
	chain := bootstrap(ctx, d, workspaceID)
	var cache VerifyCache
	for _, v := range chain {
		if v.Name() == "reacher" {
			cache = d.Cache
			break
		}
	}
	return run(ctx, workspaceID, email, chain, cache)
}

// CascadeVerifiers runs an explicit chain as given: no Reacher injection and no
// cache (Python's `verifiers=` argument).
func CascadeVerifiers(ctx context.Context, d Deps, workspaceID, email string, chain []Verifier) (Result, error) {
	if email == "" || !strings.Contains(email, "@") {
		return badSyntax(email), nil
	}
	return run(ctx, workspaceID, email, chain, nil)
}

func badSyntax(email string) Result {
	return Result{Email: email, Status: StatusInvalid, Source: "syntax", Confidence: confidenceByStatus[StatusInvalid], Detail: "bad_syntax", Attempts: []Attempt{}}
}

func bootstrap(ctx context.Context, d Deps, workspaceID string) []Verifier {
	var chain []Verifier
	if d.Reacher != nil {
		if rv := d.Reacher(ctx, workspaceID); rv != nil {
			if ok, err := rv.Available(ctx); err == nil && ok {
				chain = append(chain, rv)
			}
		}
	}
	if d.Registry != nil {
		for _, v := range d.Registry.Available(ctx) {
			if b, ok := v.(binder); ok {
				v = b.bind(d)
			}
			chain = append(chain, v)
		}
	}
	return chain
}

func run(ctx context.Context, workspaceID, email string, chain []Verifier, cache VerifyCache) (Result, error) {
	last := Result{Email: email, Status: StatusUnknown, Detail: "no_verifier", Attempts: []Attempt{}}
	for _, v := range chain {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		name := v.Name()
		if cache != nil {
			if e, hit, err := cache.Get(ctx, workspaceID, email, name); err == nil && hit {
				source := e.Source
				if source == "" {
					source = name
				}
				res := Result{Email: email, Status: e.Status, Source: source, Confidence: e.Confidence, Detail: e.Detail}
				res.Attempts = append(append([]Attempt{}, last.Attempts...), Attempt{Provider: name, Status: res.Status, Detail: "cache_hit"})
				if res.IsDefinitive() {
					return res, nil
				}
				last = res
				continue
			}
		}
		res, err := v.Verify(ctx, email)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return Result{}, cerr
			}
			kind := "error"
			var k interface{ Kind() string }
			if errors.As(err, &k) {
				kind = k.Kind()
			}
			last.Attempts = append(last.Attempts, Attempt{Provider: name, Status: "error", Detail: kind})
			continue
		}
		res.Attempts = append(append([]Attempt{}, last.Attempts...), Attempt{Provider: name, Status: res.Status, Detail: res.Detail})
		if cache != nil && !isTransient(res) {
			_ = cache.Put(ctx, workspaceID, email, name, CacheEntry{Status: res.Status, Source: res.Source, Detail: res.Detail, Confidence: res.Confidence})
		}
		if res.IsDefinitive() {
			return res, nil
		}
		last = res
	}
	return last, nil
}
