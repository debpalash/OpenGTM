package govidp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

const (
	// ConfigKey is the workspace_settings key Python's /auth/sso/* reads.
	ConfigKey = "OIDC_CONFIG"
	// SecretKey is the resolver key of the OIDC client secret.
	SecretKey = "OIDC_CLIENT_SECRET"
)

// OIDCConfig is the stored/returned config. Field order is Python's
// DEFAULT_CONFIG order. The client secret is never part of it.
type OIDCConfig struct {
	Enabled        bool     `json:"enabled"`
	EnforceSSO     bool     `json:"enforce_sso"`
	Issuer         string   `json:"issuer"`
	ClientID       string   `json:"client_id"`
	AllowedDomains []string `json:"allowed_domains"`
	AutoProvision  bool     `json:"auto_provision"`
	DefaultRole    string   `json:"default_role"`
}

// OIDCInput is the PUT /sso body (routers/governance.py OidcUpdate).
type OIDCInput struct {
	Enabled        bool     `json:"enabled"`
	EnforceSSO     bool     `json:"enforce_sso"`
	Issuer         string   `json:"issuer"`
	ClientID       string   `json:"client_id"`
	ClientSecret   string   `json:"client_secret"`
	AllowedDomains []string `json:"allowed_domains"`
	AutoProvision  bool     `json:"auto_provision"`
	DefaultRole    string   `json:"default_role"`
}

func defaultConfig() OIDCConfig {
	return OIDCConfig{AllowedDomains: []string{}, DefaultRole: "viewer"}
}

// GetConfig is oidc.get_config: stored JSON laid over the defaults. tx must be
// inside tenancy.InWorkspace(ws).
func GetConfig(ctx context.Context, tx pgx.Tx, ws string) (OIDCConfig, error) {
	raw, err := gen.New(tx).GetWorkspaceSetting(ctx, gen.GetWorkspaceSettingParams{WorkspaceID: ws, Key: ConfigKey})
	cfg := defaultConfig()
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && raw == "") {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	var kv map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &kv); err != nil {
		return defaultConfig(), errors.New("govidp: stored OIDC config is not valid JSON")
	}
	// Exact (case-sensitive) keys like json.loads; a wrongly typed value keeps the default.
	for k, dst := range map[string]any{
		"enabled": &cfg.Enabled, "enforce_sso": &cfg.EnforceSSO, "issuer": &cfg.Issuer,
		"client_id": &cfg.ClientID, "allowed_domains": &cfg.AllowedDomains,
		"auto_provision": &cfg.AutoProvision, "default_role": &cfg.DefaultRole,
	} {
		if v, ok := kv[k]; ok {
			_ = json.Unmarshal(v, dst)
		}
	}
	if cfg.AllowedDomains == nil {
		cfg.AllowedDomains = []string{}
	}
	return cfg, nil
}

// SecretConfigured is bool(get_secret(ws, OIDC_CLIENT_SECRET)): workspace,
// then global, then env. A resolver failure reports false.
func SecretConfigured(ctx context.Context, r *secrets.Resolver, ws string) bool {
	ok, _ := secretPresent(ctx, r, ws)
	return ok
}

func secretPresent(ctx context.Context, r *secrets.Resolver, ws string) (bool, error) {
	v, _, err := r.Get(ctx, ws, SecretKey)
	if err != nil {
		return false, errors.New("govidp: reading client secret failed")
	}
	return v != "", nil
}

// SaveConfig is oidc.save_config. Validation order and messages are Python's.
// The client secret is written through the resolver (encrypted, separate
// store) only when non-blank after strip, after validation and before the
// config row, and is never echoed. tx must be inside tenancy.InWorkspace(ws).
func SaveConfig(ctx context.Context, tx pgx.Tx, r *secrets.Resolver, ws string, in OIDCInput) (OIDCConfig, error) {
	clean := OIDCConfig{
		Enabled:        in.Enabled,
		EnforceSSO:     in.EnforceSSO,
		ClientID:       pyStrip(in.ClientID),
		AllowedDomains: cleanDomains(in.AllowedDomains),
		AutoProvision:  in.AutoProvision,
		DefaultRole:    in.DefaultRole,
	}
	if in.Issuer != "" {
		iss, err := ValidateIssuer(in.Issuer)
		if err != nil {
			return OIDCConfig{}, err
		}
		clean.Issuer = iss
	}
	switch clean.DefaultRole {
	case "viewer", "member", "editor":
	default:
		return OIDCConfig{}, &ValueError{"OIDC default role must be viewer, member, or editor"}
	}
	if clean.EnforceSSO && !clean.Enabled {
		return OIDCConfig{}, &ValueError{"SSO enforcement requires OIDC to be enabled"}
	}
	newSecret := pyStrip(in.ClientSecret) != ""
	if clean.Enabled {
		have := newSecret
		if !have && clean.Issuer != "" && clean.ClientID != "" {
			var err error
			if have, err = secretPresent(ctx, r, ws); err != nil {
				return OIDCConfig{}, err
			}
		}
		if clean.Issuer == "" || clean.ClientID == "" || !have {
			return OIDCConfig{}, &ValueError{"Enabled OIDC requires issuer, client_id, and client_secret"}
		}
	}
	if newSecret {
		if err := r.Set(ctx, ws, SecretKey, in.ClientSecret); err != nil {
			return OIDCConfig{}, errors.New("govidp: storing client secret failed")
		}
	}
	err := gen.New(tx).UpsertWorkspaceSetting(ctx, gen.UpsertWorkspaceSettingParams{
		WorkspaceID: ws, Key: ConfigKey, Value: pyJSON(clean),
	})
	if err != nil {
		return OIDCConfig{}, fmt.Errorf("govidp: saving OIDC config: %w", err)
	}
	return clean, nil
}

func cleanDomains(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, d := range in {
		if d = strings.ToLower(pyStrip(d)); d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out) // UTF-8 byte order == Python code point order
	return out
}

// pyJSON renders cfg exactly like json.dumps(clean, separators=(",", ":")):
// Python key order, ensure_ascii escapes, no HTML escaping.
func pyJSON(c OIDCConfig) string {
	var b strings.Builder
	b.WriteString(`{"enabled":`)
	b.WriteString(boolStr(c.Enabled))
	b.WriteString(`,"enforce_sso":` + boolStr(c.EnforceSSO))
	b.WriteString(`,"issuer":`)
	pyString(&b, c.Issuer)
	b.WriteString(`,"client_id":`)
	pyString(&b, c.ClientID)
	b.WriteString(`,"allowed_domains":[`)
	for i, d := range c.AllowedDomains {
		if i > 0 {
			b.WriteByte(',')
		}
		pyString(&b, d)
	}
	b.WriteString(`],"auto_provision":` + boolStr(c.AutoProvision))
	b.WriteString(`,"default_role":`)
	pyString(&b, c.DefaultRole)
	b.WriteByte('}')
	return b.String()
}

func boolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func pyString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= ' ' && r <= '~':
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
		}
	}
	b.WriteByte('"')
}
