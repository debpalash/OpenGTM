package rpclocal

import (
	"context"
	"log/slog"
	"os"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/scrape"
)

// Setup is the per-process wiring of the in-process providers.
type Setup struct {
	Getenv      func(string) string // nil: os.Getenv
	AppEnv      string              // egress bench/test allow-list scope
	EgressAllow []string            // EGRESS_ALLOW_HOSTS
	Py          PyCaller            // the Python worker client, or nil
	Secrets     Secrets             // credential resolver (LinkedIn session, keyed providers), or nil
	Log         *slog.Logger
}

// Configure reads GO_LOCAL_PROVIDERS, the browser service and leadweb
// settings, installs the provider gate and the providers/scrape seams, and
// returns the composite caller. It returns nil (and no error) when there is
// neither a Python worker nor a locally served name, so callers that test a
// nil caller keep their "Python unavailable" behaviour.
func Configure(s Setup) (*Caller, error) {
	get := s.Getenv
	if get == nil {
		get = os.Getenv
	}
	cfg, err := config.LocalProvidersFrom(get)
	if err != nil {
		return nil, err
	}
	lw, err := config.LeadwebFrom(get)
	if err != nil {
		return nil, err
	}
	providers.SetLocalGate(cfg.Local.Enabled)
	bc := browserclient.New(cfg.BrowserURL, cfg.BrowserToken.Reveal(), nil)
	web := leadweb.New(lw, bc, s.AppEnv, s.EgressAllow)

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
	if s.Secrets != nil {
		sec := s.Secrets
		d.LinkedInCookies = func(ctx context.Context) ([]browserclient.Cookie, error) {
			liAt, _, err := sec.Get(ctx, "", "LINKEDIN_LI_AT_COOKIE")
			if err != nil || liAt == "" {
				return nil, err
			}
			cs := []browserclient.Cookie{{Name: "li_at", Value: liAt, Domain: ".linkedin.com", Path: "/", Secure: true, HTTPOnly: true}}
			if js, _, err := sec.Get(ctx, "", "LINKEDIN_JSESSIONID"); err == nil && js != "" {
				cs = append(cs, browserclient.Cookie{Name: "JSESSIONID", Value: js, Domain: ".www.linkedin.com", Path: "/", Secure: true})
			}
			return cs, nil
		}
	}
	scrape.Configure(d)
	if s.Py == nil && !cfg.Local.Any() {
		return nil, nil
	}
	if s.Log != nil && cfg.Local.Any() {
		s.Log.Info("in-process providers on (GO_LOCAL_PROVIDERS)", "browser_service", bc.Configured())
	}
	return New(Deps{Py: s.Py, Local: cfg.Local.Enabled, HTTP: egress.NewClientForEnv(s.AppEnv, s.EgressAllow),
		Secrets: s.Secrets, Log: s.Log}), nil
}
