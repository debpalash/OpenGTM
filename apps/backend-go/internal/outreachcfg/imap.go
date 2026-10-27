package outreachcfg

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// IMAP is the inbound bounce/complaint mailbox configuration.
type IMAP struct {
	Host     string
	Port     int
	User     string
	Password config.Secret
	Folder   string
	UseSSL   bool
	// Global marks a mailbox whose host and port come from the global/env settings: every workspace without
	// IMAP settings of its own resolves to this same mailbox (plan 09 final-fix follow-up M2).
	Global bool
}

// Configured mirrors is_imap_configured.
func (c IMAP) Configured() bool { return c.Host != "" && c.User != "" && c.Password != "" }

// LoadIMAP resolves the IMAP config; user and password default to the SMTP
// credentials. Any error yields the zero IMAP{}.
func LoadIMAP(ctx context.Context, r *secrets.Resolver, workspaceID string) IMAP {
	g := &getter{ctx: ctx, r: r, ws: workspaceID}
	user, userWS := g.getSrc("IMAP_USER", "")
	if user == "" {
		user, userWS = g.getSrc("SMTP_EMAIL", "")
	}
	pw, pwWS := g.getSrc("IMAP_PASSWORD", "")
	if pw == "" {
		pw, pwWS = g.getSrc("SMTP_PASSWORD", "")
	}
	host, hostWS := g.getSrc("IMAP_HOST", "")
	_, portWS := g.getSrc("IMAP_PORT", "993")
	if hostWS || portWS {
		// global credentials only travel to the global host and port (plan 09 final security H1)
		if (user != "" && !userWS) || (pw != "" && !pwWS) {
			user, pw = "", ""
		}
	} else {
		// a global host: only platform-level credentials may go to it. A workspace's own SMTP or IMAP
		// credentials never travel to the operator's mailbox host.
		user, pw = "", ""
		gg := &getter{ctx: ctx, r: r, ws: ""}
		if user = gg.get("IMAP_USER", ""); user == "" {
			user = gg.get("SMTP_EMAIL", "")
		}
		if pw = gg.get("IMAP_PASSWORD", ""); pw == "" {
			pw = gg.get("SMTP_PASSWORD", "")
		}
		g.bad = g.bad || gg.bad
	}
	c := IMAP{
		Host:     host,
		Port:     g.num("IMAP_PORT", "993"),
		User:     user,
		Password: config.Secret(pw),
		Folder:   g.get("IMAP_FOLDER", "INBOX"),
		UseSSL:   g.get("IMAP_USE_SSL", "1") == "1",
		Global:   host != "" && !hostWS && !portWS,
	}
	if g.bad {
		return IMAP{}
	}
	return c
}
