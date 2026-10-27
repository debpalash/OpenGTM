// Package outreachcfg loads per-workspace outreach SMTP, IMAP and footer
// settings (port of services/outreach/sender.py get_smtp_config and
// get_imap_config). Passwords are config.Secret and never appear in errors.
package outreachcfg

import (
	"context"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/smtpmail"
)

// SMTP is the resolved sender configuration.
type SMTP struct {
	Host       string
	Port       int
	Email      string
	Password   config.Secret
	FromName   string
	UseTLS     bool
	MaxPerHour int
	// Mixed marks a config that mixes workspace and platform levels (a workspace host or port with platform
	// credentials, or a workspace email or password without the full workspace bundle), or an invalid stored
	// email; the credentials are withheld and Configured is false.
	Mixed bool
	// HostPlatform and EmailPlatform mark values that resolved from the global/env level; they are operator
	// settings and are not shown to workspace members.
	HostPlatform, EmailPlatform bool
}

// Configured mirrors is_smtp_configured.
func (c SMTP) Configured() bool {
	return !c.Mixed && c.Host != "" && c.Email != "" && c.Password != ""
}

// ConfigError is the admin-facing reason a Mixed config is refused ("" otherwise).
func (c SMTP) ConfigError() string {
	if c.Mixed {
		return mixedErr("SMTP")
	}
	return ""
}

func mixedErr(kind string) string {
	return kind + " settings mix workspace and platform levels; set the host, port, username and password " +
		"for this workspace together (platform credentials are never used with a workspace host, and a workspace " +
		"username or password is never sent to the platform host)"
}

// getter resolves one key with a default (Python get_secret(ws, key, default)).
// ok=false means the lookup failed; callers return the zero config, like the
// Python blanket except.
type getter struct {
	ctx context.Context
	r   *secrets.Resolver
	ws  string
	bad bool
}

func (g *getter) get(key, def string) string {
	v, _ := g.getSrc(key, def)
	return v
}

// getSrc is get plus whether the value came from the workspace level.
func (g *getter) getSrc(key, def string) (string, bool) {
	v, src, err := g.r.Get(g.ctx, g.ws, key)
	if err != nil {
		g.bad = true
		return def, false
	}
	if v == "" {
		return def, false
	}
	return v, src == secrets.SourceWorkspace
}

// globalSet reports whether key has a value at the global/env level (not the workspace).
func (g *getter) globalSet(key string) bool {
	v, src, err := g.r.Get(g.ctx, g.ws, key)
	return err == nil && v != "" && src != secrets.SourceWorkspace
}

// num mirrors int(_g(key, def) or def); a parse error poisons the load.
func (g *getter) num(key, def string) int {
	n, err := strconv.Atoi(strings.TrimSpace(g.get(key, def)))
	if err != nil {
		g.bad = true
	}
	return n
}

// LoadSMTP resolves the workspace SMTP config (workspace secret, then global,
// then env). Any lookup or parse error yields the zero SMTP{}.
func LoadSMTP(ctx context.Context, r *secrets.Resolver, workspaceID string) SMTP {
	g := &getter{ctx: ctx, r: r, ws: workspaceID}
	host, hostWS := g.getSrc("SMTP_HOST", "")
	_, portWS := g.getSrc("SMTP_PORT", "587")
	portGlobal := g.globalSet("SMTP_PORT")
	email, emailWS := g.getSrc("SMTP_EMAIL", "")
	pw, pwWS := g.getSrc("SMTP_PASSWORD", "")
	mixed := false
	switch {
	case hostWS || portWS:
		// global credentials only travel to the global host and port
		if (email != "" && !emailWS) || (pw != "" && !pwWS) {
			mixed = true
		}
	case emailWS || pwWS:
		// a workspace that names its own mailbox or password must bring the whole bundle: otherwise the
		// platform host would carry its mailbox as From or its password would reach the platform relay
		if !(emailWS && pwWS && hostWS) || portGlobal {
			mixed = true
		}
	}
	if !mixed && emailWS && !smtpmail.ValidRecipient(email) {
		// values stored before the route validated them (a list, a display name) are never used as From
		mixed = true
	}
	if mixed {
		email, pw = "", ""
	}
	c := SMTP{
		Host:         host,
		Port:         g.num("SMTP_PORT", "587"),
		Email:        email,
		Password:     config.Secret(pw),
		Mixed:        mixed,
		HostPlatform: host != "" && !hostWS, EmailPlatform: email != "" && !emailWS,
		FromName:   g.get("SMTP_FROM_NAME", "OpenGTM"),
		UseTLS:     g.get("SMTP_USE_TLS", "1") == "1",
		MaxPerHour: g.num("SMTP_MAX_PER_HOUR", "50"),
	}
	if g.bad {
		return SMTP{}
	}
	return c
}

// SMTPUpdate carries the optional fields of PUT /smtp/config; nil means
// "leave unchanged".
type SMTPUpdate struct {
	Host       *string
	Port       *int
	Email      *string
	Password   *config.Secret
	FromName   *string
	MaxPerHour *int
	UseTLS     *bool
}

// Setter is the write side of the secret resolver (*secrets.Resolver).
type Setter interface {
	Set(ctx context.Context, ws, key, value string) error
}

// SetSMTP stores each non-nil field as an encrypted per-workspace secret.
// Validation of host and port belongs to the route. Errors never include values.
func SetSMTP(ctx context.Context, s Setter, workspaceID string, u SMTPUpdate) error {
	type kv struct{ k, v string }
	var items []kv
	if u.Host != nil {
		items = append(items, kv{"SMTP_HOST", *u.Host})
	}
	if u.Port != nil {
		items = append(items, kv{"SMTP_PORT", strconv.Itoa(*u.Port)})
	}
	if u.Email != nil {
		items = append(items, kv{"SMTP_EMAIL", *u.Email})
	}
	if u.Password != nil {
		items = append(items, kv{"SMTP_PASSWORD", u.Password.Reveal()})
	}
	if u.FromName != nil {
		items = append(items, kv{"SMTP_FROM_NAME", *u.FromName})
	}
	if u.MaxPerHour != nil {
		items = append(items, kv{"SMTP_MAX_PER_HOUR", strconv.Itoa(*u.MaxPerHour)})
	}
	if u.UseTLS != nil {
		v := "0"
		if *u.UseTLS {
			v = "1"
		}
		items = append(items, kv{"SMTP_USE_TLS", v})
	}
	for _, it := range items {
		if err := s.Set(ctx, workspaceID, it.k, it.v); err != nil {
			return err
		}
	}
	return nil
}
