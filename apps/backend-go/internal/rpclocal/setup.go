package rpclocal

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/scrape"
	secretstore "github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Setup is the per-process wiring of the in-process providers.
type Setup struct {
	Getenv      func(string) string // nil: config.Getenv (GO_ALL_NATIVE resolved)
	AppEnv      string              // egress bench/test allow-list scope
	EgressAllow []string            // EGRESS_ALLOW_HOSTS
	Py          PyCaller            // the Python worker client, or nil
	Secrets     Secrets             // credential resolver (LinkedIn session, keyed providers), or nil
	Log         *slog.Logger
	// Web and Browser are the process's shared leadweb and browser-service client (one rate
	// limiter, breaker and search cache per process); nil builds them from the environment.
	Web     *leadweb.Web
	Browser *browserclient.Client
	// Runner runs registered Go providers (the binary's providerrun.Runner); nil builds one on
	// Pool (enrichment cache + provider_stats ledger) — without a Pool, settings and keys only.
	Runner Runner
	Pool   *pgxpool.Pool
}

// Configure reads GO_LOCAL_PROVIDERS, the browser service and leadweb
// settings, installs the provider gate and the providers/scrape seams, and
// returns the composite caller. It returns nil (and no error) when there is
// neither a Python worker nor a locally served name, so callers that test a
// nil caller keep their "Python unavailable" behaviour.
func Configure(s Setup) (*Caller, error) {
	get := s.Getenv
	if get == nil {
		get = config.Getenv
	}
	cfg, err := config.LocalProvidersFrom(get)
	if err != nil {
		return nil, err
	}
	providers.SetLocalGate(cfg.Local.Enabled)
	bc, web := s.Browser, s.Web
	if bc == nil {
		bc = browserclient.New(cfg.BrowserURL, cfg.BrowserToken.Reveal(), nil)
	}
	if web == nil {
		lw, err := config.LeadwebFrom(get)
		if err != nil {
			return nil, err
		}
		web = leadweb.New(lw, bc, s.AppEnv, s.EgressAllow)
	}

	mail := emailverify.NewDeps()
	mail.SMTPDisabled = get("LEADGEN_SMTP_VERIFY") == "0"
	mail.HeloHost, mail.MailFrom = get("LEADGEN_SMTP_HELO"), get("LEADGEN_SMTP_MAIL_FROM")
	dial := egress.NewDialContext(s.AppEnv, s.EgressAllow)
	mail.NewSession = func() emailverify.SMTPSession { return emailverify.NewNetSession(dial, mail.Timeout()) }

	d := scrape.Deps{Search: scrape.LiveSearch{S: web.Search}, Fetch: web.Fetch, Mail: scrape.LiveMail{D: mail},
		StaffSpyEnabled: cfg.StaffSpyEnabled}
	if bc.Configured() {
		d.Browser = bc
	}
	var session func(ctx context.Context, ws string) []string
	if s.Secrets != nil {
		sec := s.Secrets
		// Credential rule: the LinkedIn session is the calling workspace's own secret, sent to
		// the browser service for that one request; an operator / env / API-served value is
		// never used (no workspace secret -> staffspy unavailable).
		wsOnly := func(ctx context.Context, ws, key string) (string, error) {
			if ws == "" {
				return "", nil
			}
			v, src, err := sec.Get(ctx, ws, key)
			if err != nil || src != secretstore.SourceWorkspace {
				return "", err
			}
			return v, nil
		}
		d.LinkedInCookies = func(ctx context.Context, ws string) ([]browserclient.Cookie, error) {
			liAt, err := wsOnly(ctx, ws, "LINKEDIN_LI_AT_COOKIE")
			if err != nil || liAt == "" {
				return nil, err
			}
			cs := []browserclient.Cookie{{Name: "li_at", Value: liAt, Domain: ".linkedin.com", Path: "/", Secure: true, HTTPOnly: true}}
			if js, err := wsOnly(ctx, ws, "LINKEDIN_JSESSIONID"); err == nil && js != "" {
				cs = append(cs, browserclient.Cookie{Name: "JSESSIONID", Value: js, Domain: ".www.linkedin.com", Path: "/", Secure: true})
			}
			return cs, nil
		}
		// Every provider-call output and error is scrubbed of the workspace's session values.
		session = func(ctx context.Context, ws string) []string {
			var out []string
			for _, k := range []string{"LINKEDIN_LI_AT_COOKIE", "LINKEDIN_JSESSIONID"} {
				if v, err := wsOnly(ctx, ws, k); err == nil && v != "" {
					out = append(out, v)
				}
			}
			return out
		}
	}
	scrape.Configure(d)
	if s.Py == nil && !cfg.Local.Any() {
		return nil, nil
	}
	if s.Log != nil && cfg.Local.Any() {
		s.Log.Info("in-process providers on (GO_LOCAL_PROVIDERS)", "browser_service", bc.Configured())
	}
	strict := egress.NewClientForEnv(s.AppEnv, s.EgressAllow)
	runner := s.Runner
	if runner == nil {
		var sec providerrun.Secrets = noSecrets{}
		if s.Secrets != nil {
			sec = s.Secrets
		}
		runner = providerrun.New(providerrun.Deps{Pool: s.Pool, HTTP: strict, Secrets: sec, Log: s.Log})
	}
	return New(Deps{Py: s.Py, Local: cfg.Local.Enabled, HTTP: strict, Runner: runner,
		Secrets: s.Secrets, Scrub: session, Log: s.Log}), nil
}
