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
}

// Configured mirrors is_imap_configured.
func (c IMAP) Configured() bool { return c.Host != "" && c.User != "" && c.Password != "" }

// LoadIMAP resolves the IMAP config; user and password default to the SMTP
// credentials. Any error yields the zero IMAP{}.
func LoadIMAP(ctx context.Context, r *secrets.Resolver, workspaceID string) IMAP {
	g := &getter{ctx: ctx, r: r, ws: workspaceID}
	user := g.get("IMAP_USER", "")
	if user == "" {
		user = g.get("SMTP_EMAIL", "")
	}
	pw := g.get("IMAP_PASSWORD", "")
	if pw == "" {
		pw = g.get("SMTP_PASSWORD", "")
	}
	c := IMAP{
		Host:     g.get("IMAP_HOST", ""),
		Port:     g.num("IMAP_PORT", "993"),
		User:     user,
		Password: config.Secret(pw),
		Folder:   g.get("IMAP_FOLDER", "INBOX"),
		UseSSL:   g.get("IMAP_USE_SSL", "1") == "1",
	}
	if g.bad {
		return IMAP{}
	}
	return c
}
