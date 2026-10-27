package config

import (
	"fmt"
	"regexp"
	"strings"
)

// LocalProviders is GO_LOCAL_PROVIDERS (plan 15 WP-PROVIDERS): the provider
// and RPC names served in process before the Python worker. Empty (the
// default) keeps every Python-only provider on the worker; "all" serves every
// name that has a Go implementation.
type LocalProviders struct {
	all   bool
	names map[string]bool
}

// Enabled reports whether name is listed (case-insensitive).
func (l LocalProviders) Enabled(name string) bool {
	return l.all || l.names[strings.ToLower(name)]
}

// Any reports whether anything is listed.
func (l LocalProviders) Any() bool { return l.all || len(l.names) > 0 }

// LocalProvidersSettings configure internal/rpclocal and providers/scrape.
type LocalProvidersSettings struct {
	Local           LocalProviders
	BrowserURL      string // BROWSER_SERVICE_URL (empty: no browser service; page work then fails)
	BrowserToken    Secret // BROWSER_SERVICE_TOKEN
	StaffSpyEnabled bool   // STAFFSPY_ENABLED (legacy opt-in; LinkedIn ToS)
}

var providerNameRe = regexp.MustCompile(`^[a-z0-9_.]{1,200}$`)

// LocalProvidersFrom reads the settings. A malformed GO_LOCAL_PROVIDERS is an
// error, like every GO_* flag.
func LocalProvidersFrom(get func(string) string) (LocalProvidersSettings, error) {
	get = AllNative(get) // GO_LOCAL_PROVIDERS is retired: always "all"
	s := LocalProvidersSettings{BrowserURL: strings.TrimSpace(get("BROWSER_SERVICE_URL")),
		BrowserToken: Secret(strings.TrimSpace(get("BROWSER_SERVICE_TOKEN")))}
	switch strings.ToLower(strings.TrimSpace(get("STAFFSPY_ENABLED"))) {
	case "1", "true", "yes", "on":
		s.StaffSpyEnabled = true
	}
	names := map[string]bool{}
	for _, p := range strings.Split(get("GO_LOCAL_PROVIDERS"), ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		switch {
		case p == "":
		case p == "all":
			s.Local.all = true
		case providerNameRe.MatchString(p):
			names[p] = true
		default:
			return s, fmt.Errorf("GO_LOCAL_PROVIDERS: invalid provider name %q", p)
		}
	}
	if s.Local.all && len(names) > 0 {
		return s, fmt.Errorf("GO_LOCAL_PROVIDERS: \"all\" cannot be combined with names")
	}
	s.Local.names = names
	return s, nil
}
