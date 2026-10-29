package emailverify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/contact"
)

// KeyFunc resolves one credential for a workspace (credentials.Client.Get).
// Credentials are per workspace and never shared across workspaces.
type KeyFunc func(ctx context.Context, workspaceID, envVar string) (string, error)

// ProviderCaller is the provider caller: the rpclocal
// composite that serves GO_LOCAL_PROVIDERS names (website_scraper) in process.
type ProviderCaller interface {
	Call(ctx context.Context, call provcall.Call) (provcall.Result, error)
}

// LiveConfig wires the production Deps.
type LiveConfig struct {
	Pool        *pgxpool.Pool
	AppEnv      string       // egress allowlist is honoured only for test/bench
	EgressAllow []string     // egress allow hosts (bench/test only)
	HTTP        *http.Client // egress-guarded client used for Reacher and the vendor finders
	Keys        KeyFunc
	// ReacherSettings returns the GLOBAL operator Reacher settings (REACHER_URL,
	// REACHER_ENABLED, REACHER_TIMEOUT), never workspace-controlled: a tenant must
	// not be able to point outbound requests at an internal host.
	ReacherSettings func(ctx context.Context, workspaceID string) map[string]any
	Py              ProviderCaller // nil disables the website finder
	SMTPDisabled    bool           // LEADGEN_SMTP_VERIFY=0
	HeloHost        string
	MailFrom        string
}

// NewLive returns the production Deps: system DNS, SMTP over the egress dial
// guard (private MX hosts refused at dial time), the Postgres per-workspace
// cache, Reacher first when configured, and the website/hunter/snovio finders.
func NewLive(cfg LiveConfig) Deps {
	d := NewDeps()
	d.SMTPDisabled, d.HeloHost, d.MailFrom = cfg.SMTPDisabled, cfg.HeloHost, cfg.MailFrom
	dial := egress.NewDialContext(cfg.AppEnv, cfg.EgressAllow)
	d.NewSession = func() SMTPSession { return NewNetSession(dial, d.Timeout()) }
	if cfg.Pool != nil {
		d.Cache = NewPGCache(cfg.Pool, nil)
	}
	d.Reacher = func(_ context.Context, ws string) Verifier {
		return &reacherVerifier{ws: ws, cfg: cfg}
	}
	d.Finders = []Finder{
		&WebsiteFinder{Py: cfg.Py},
		&ProviderFinder{Label: "hunter", Provider: "hunter_io", HTTP: cfg.HTTP, Keys: cfg.Keys},
		&ProviderFinder{Label: "snovio", Provider: "snovio", HTTP: cfg.HTTP, Keys: cfg.Keys},
	}
	return d
}

// ---- Reacher ----------------------------------------------------------------

type reacherVerifier struct {
	ws  string
	cfg LiveConfig
}

func (*reacherVerifier) Name() string { return "reacher" }

func (r *reacherVerifier) input(ctx context.Context, email string) providers.Input {
	in := providers.Input{Lead: map[string]any{"email": email}, Field: "email"}
	if r.cfg.ReacherSettings != nil {
		in.Settings = r.cfg.ReacherSettings(ctx, r.ws)
	}
	return in
}

func (r *reacherVerifier) Available(ctx context.Context) (bool, error) {
	return contact.ReacherAvailable(r.input(ctx, "")), nil
}

// Verify never reports a vendor failure as an error: the Reacher provider maps
// them to an unknown result (and feeds its circuit breaker). Only a cancelled
// context or a credential lookup failure surfaces.
func (r *reacherVerifier) Verify(ctx context.Context, email string) (Result, error) {
	p, ok := providers.Get("reacher")
	if !ok {
		return Result{}, errors.New("reacher provider is not registered")
	}
	out, err := p.Enrich(ctx, r.cfg.HTTP, func() (string, error) {
		if r.cfg.Keys == nil {
			return "", nil
		}
		return r.cfg.Keys(ctx, r.ws, "REACHER_API_KEY")
	}, r.input(ctx, email))
	if err != nil {
		return Result{}, err
	}
	status, _ := out.Value.(string)
	detail := ""
	if out.Extra != nil {
		if v, ok := out.Extra.Get("detail"); ok {
			detail, _ = v.(string)
		}
	}
	return Result{Email: email, Status: status, Source: "reacher", Confidence: out.Confidence, Detail: detail, Attempts: []Attempt{}}, nil
}

// ---- finders ----------------------------------------------------------------

// ProviderFinder runs a registered HTTP provider (hunter_io, snovio) as a
// waterfall step. Label is the step name Python records ("hunter", "snovio").
type ProviderFinder struct {
	Label    string
	Provider string
	HTTP     *http.Client
	Keys     KeyFunc
}

func (f *ProviderFinder) Name() string { return f.Label }

func (f *ProviderFinder) envs() []string {
	p, ok := providers.Get(f.Provider)
	if !ok {
		return nil
	}
	if k, ok := p.(providers.Keyed); ok {
		return k.KeyEnv()
	}
	return nil
}

// Available is the credential gate: every required credential is present for
// this workspace (a lookup failure counts as absent, like Python's bare except).
func (f *ProviderFinder) Available(ctx context.Context, workspaceID string) bool {
	envs := f.envs()
	if len(envs) == 0 || f.Keys == nil {
		return false
	}
	for _, e := range envs {
		if v, err := f.Keys(ctx, workspaceID, e); err != nil || v == "" {
			return false
		}
	}
	return true
}

func (f *ProviderFinder) Find(ctx context.Context, workspaceID string, lead map[string]any, _ string) (string, map[string]string, error) {
	p, ok := providers.Get(f.Provider)
	if !ok {
		return "", nil, errors.New("provider " + f.Provider + " is not registered")
	}
	envs := f.envs()
	key := func() (string, error) {
		vals := make([]string, 0, len(envs))
		for _, e := range envs {
			v, err := f.Keys(ctx, workspaceID, e)
			if err != nil {
				return "", err
			}
			vals = append(vals, v)
		}
		if len(vals) == 1 {
			return vals[0], nil
		}
		return providers.JoinKeys(vals...), nil
	}
	out, err := p.Enrich(ctx, f.HTTP, key, providers.Input{Lead: lead, Field: "email"})
	if err != nil {
		return "", nil, err
	}
	if out.Error == "" && out.Extra != nil {
		if v, ok := out.Extra.Get("email"); ok && providers.Truthy(v) {
			email, _ := v.(string)
			extra := map[string]string{}
			for _, k := range []string{"contact_person", "contact_title"} {
				if cv, ok := out.Extra.Get(k); ok && providers.Truthy(cv) {
					extra[k], _ = cv.(string)
				}
			}
			return email, extra, nil
		}
	}
	if out.Error != "" {
		return "", nil, errors.New(out.Error)
	}
	return "", nil, nil
}

// WebsiteProvider is the provider that scrapes a company website: Go
// (providers/scrape over the browser service).
const WebsiteProvider = "website_scraper"

// WebsiteFinder is the free first step: scrape the lead's website for a
// contact email through the provider-call RPC. The provider reports at most three
// addresses (email, secondary_emails); the first usable one wins.
type WebsiteFinder struct {
	Py      ProviderCaller
	Timeout time.Duration
}

func (*WebsiteFinder) Name() string { return "website" }

func (f *WebsiteFinder) Available(context.Context, string) bool { return f.Py != nil }

func (f *WebsiteFinder) Find(ctx context.Context, workspaceID string, lead map[string]any, _ string) (string, map[string]string, error) {
	if site, _ := lead["website"].(string); site == "" {
		return "", nil, nil
	}
	input, err := json.Marshal(lead)
	if err != nil {
		return "", nil, err
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	res, err := f.Py.Call(ctx, provcall.Call{
		CallID: "emailwf-" + hex.EncodeToString(id), Provider: WebsiteProvider, WorkspaceID: workspaceID,
		Input: input, Timeout: f.Timeout,
	})
	if err != nil {
		return "", nil, err
	}
	if res.Status != "ok" {
		msg := res.Error
		if msg == "" {
			msg = res.Status
		}
		return "", nil, errors.New(msg)
	}
	var o struct {
		Fields map[string]any `json:"fields"`
	}
	if json.Unmarshal(res.Output, &o) != nil {
		return "", nil, nil
	}
	cands := []string{}
	if e, ok := o.Fields["email"].(string); ok {
		cands = append(cands, e)
	}
	if s, ok := o.Fields["secondary_emails"].(string); ok {
		cands = append(cands, strings.Split(s, "|")...)
	}
	for _, e := range cands {
		if IsUsableEmail(e) {
			return e, nil, nil
		}
	}
	return "", nil, nil
}
