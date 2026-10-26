// Package destvalidate ports the audience-destination request models and
// config validation (apps/api/routers/destinations.py): config rules, body
// decoding with FastAPI-shaped 422 errors, history cursors and the Slack
// message template.
package destvalidate

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// Types are the supported destination types, in Python's declaration order.
var Types = []string{
	"webhook", "hubspot", "salesforce", "warehouse_http",
	"meta_ads", "google_ads", "linkedin_ads", "instantly", "smartlead",
	"google_sheets", "airtable", "slack",
}

// IsType reports whether t is a supported (already lower-cased) type.
func IsType(t string) bool {
	for _, x := range Types {
		if x == t {
			return true
		}
	}
	return false
}

// ConfigError is a config validation failure; the REST layer answers 422 with
// detail = Error().
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }

func fail(format string, a ...any) error { return &ConfigError{Msg: fmt.Sprintf(format, a...)} }

// truthy is Python truthiness for decoded JSON.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	if n, ok := v.(interface{ String() string }); ok { // json.Number
		f, ok := colgraph.PyParseFloat(n.String())
		return !ok || f != 0
	}
	return true
}

// strOr is str(v or "").
func strOr(v any) string {
	if !truthy(v) {
		return ""
	}
	return colgraph.PyStr(v)
}

func present(v any) bool { return colgraph.PyStrip(strOr(v)) != "" }

func scanCredentialKeys(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, nested := range t {
			switch colgraph.PyLower(k) {
			case "token", "api_key", "secret", "password":
				return true
			}
			if scanCredentialKeys(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range t {
			if scanCredentialKeys(nested) {
				return true
			}
		}
	}
	return false
}

func unknownKeys(cfg map[string]any, allowed ...string) []string {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var out []string
	for k := range cfg {
		if !ok[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateConfig is Python's _validate_config plus the D4 ad id rules and the
// D6 Slack template rule. It returns cfg unchanged. The workspace-secret
// existence check (header_secret_ref, webhook_secret_ref) is a separate step.
// A type outside Types gets only the credential scan, as in Python.
func ValidateConfig(destType string, cfg map[string]any) (map[string]any, error) {
	if scanCredentialKeys(cfg) {
		return nil, fail("credentials must use workspace secrets, not destination config")
	}
	unsupported := func(label string, allowed ...string) error {
		if u := unknownKeys(cfg, allowed...); len(u) > 0 {
			return fail("unsupported %s config: %s", label, strings.Join(u, ", "))
		}
		return nil
	}
	switch destType {
	case "webhook":
		if err := unsupported("webhook", "url", "method", "header_secret_ref", "header_name"); err != nil {
			return nil, err
		}
		u, err := pyURLParse(strOr(cfg["url"]))
		if err != nil {
			return nil, err
		}
		if (u.scheme != "http" && u.scheme != "https") || u.hostname == "" || u.hasUserinfo() {
			return nil, fail("webhook requires an http(s) URL without embedded credentials")
		}
		m := cfg["method"]
		method := "POST"
		if truthy(m) {
			method = colgraph.PyStr(m)
		}
		switch strings.ToUpper(method) {
		case "POST", "PUT", "PATCH":
		default:
			return nil, fail("webhook method must be POST, PUT, or PATCH")
		}
	case "warehouse_http":
		if err := unsupported("warehouse", "url", "header_secret_ref", "header_name", "dataset", "mode"); err != nil {
			return nil, err
		}
		u, err := pyURLParse(strOr(cfg["url"]))
		if err != nil {
			return nil, err
		}
		if u.scheme != "https" || u.hostname == "" || u.hasUserinfo() {
			return nil, fail("warehouse HTTP destination requires an HTTPS URL without embedded credentials")
		}
		if !present(cfg["header_secret_ref"]) {
			return nil, fail("warehouse HTTP destination requires header_secret_ref")
		}
		mode := "snapshot"
		if truthy(cfg["mode"]) {
			mode = colgraph.PyStr(cfg["mode"])
		}
		if mode != "snapshot" && mode != "upsert" {
			return nil, fail("warehouse mode must be snapshot or upsert")
		}
	case "meta_ads", "google_ads", "linkedin_ads":
		if b, _ := cfg["consent_attested"].(bool); !b || !present(cfg["consent_source"]) {
			return nil, fail("ad destinations require consent_attested=true and a consent_source")
		}
		required := map[string][]string{
			"meta_ads":     {"custom_audience_id"},
			"google_ads":   {"customer_id", "user_list_id"},
			"linkedin_ads": {"segment_id"},
		}[destType]
		var missing []string
		for _, r := range required { // already sorted
			if !present(cfg[r]) {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			return nil, fail("missing ad destination config: %s", strings.Join(missing, ", "))
		}
		allowed := append(append([]string{}, required...), "consent_attested", "consent_source", "api_version", "login_customer_id")
		if u := unknownKeys(cfg, allowed...); len(u) > 0 {
			return nil, fail("unsupported ad destination config: %s", strings.Join(u, ", "))
		}
		if err := validateAdIDs(cfg); err != nil {
			return nil, err
		}
	case "instantly", "smartlead":
		allowed := []string{"campaign_id", "settings"}
		if destType == "instantly" {
			allowed = []string{"campaign_id", "skip_if_in_campaign"}
		}
		if err := unsupported(destType, allowed...); err != nil {
			return nil, err
		}
		if !present(cfg["campaign_id"]) {
			return nil, fail("%s campaign_id is required", destType)
		}
		if s, ok := cfg["settings"]; destType == "smartlead" && ok && s != nil {
			if _, isObj := s.(map[string]any); !isObj {
				return nil, fail("smartlead settings must be an object")
			}
		}
	case "google_sheets":
		if err := unsupported("google_sheets", "spreadsheet_id", "range", "columns"); err != nil {
			return nil, err
		}
		if !present(cfg["spreadsheet_id"]) {
			return nil, fail("google_sheets spreadsheet_id is required")
		}
		cols, isList := cfg["columns"].([]any)
		okCols := isList && len(cols) >= 1 && len(cols) <= 100
		if okCols {
			for _, c := range cols {
				if s, isStr := c.(string); !isStr || colgraph.PyStrip(s) == "" {
					okCols = false
					break
				}
			}
		}
		if !okCols {
			return nil, fail("google_sheets columns must contain 1 to 100 field names")
		}
	case "airtable":
		if err := unsupported("airtable", "base_id", "table", "idempotency_field", "typecast"); err != nil {
			return nil, err
		}
		var missing []string
		for _, f := range []string{"base_id", "table"} {
			if !present(cfg[f]) {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			return nil, fail("missing airtable config: %s", strings.Join(missing, ", "))
		}
		idem := "OpenGTM ID"
		if truthy(cfg["idempotency_field"]) {
			idem = colgraph.PyStr(cfg["idempotency_field"])
		}
		if colgraph.PyStrip(idem) == "" {
			return nil, fail("airtable idempotency_field cannot be blank")
		}
	case "slack":
		if err := unsupported("slack", "webhook_secret_ref", "message_template"); err != nil {
			return nil, err
		}
		if !present(cfg["webhook_secret_ref"]) {
			return nil, fail("slack webhook_secret_ref is required")
		}
		tpl := strOr(cfg["message_template"])
		if n := len([]rune(tpl)); n > 2000 {
			return nil, fail("slack message_template may not exceed 2000 characters")
		}
		if CheckSlackTemplate(tpl) != nil {
			return nil, fail("slack message_template uses unsupported fields")
		}
	}
	return cfg, nil
}

var (
	adIDRe      = regexp.MustCompile(`^[0-9A-Za-z_.-]{1,64}$`)
	adVersionRe = regexp.MustCompile(`^(v[0-9]{1,2}(\.[0-9])?|[0-9]{6})$`)
)

// validateAdIDs (D4): ids end up in request paths, so they must be plain path
// segments. Python interpolated them unvalidated. Ids that are only dots are
// rejected too ("." and ".." are path traversal segments).
func validateAdIDs(cfg map[string]any) error {
	for _, k := range []string{"customer_id", "user_list_id", "login_customer_id", "custom_audience_id", "segment_id", "api_version"} {
		v, ok := cfg[k]
		if !ok || v == nil {
			continue
		}
		var s string
		switch t := v.(type) {
		case string:
			s = t
		case interface{ String() string }: // json.Number
			s = t.String()
		default:
			return fail("invalid ad destination config: %s", k)
		}
		if k == "login_customer_id" && s == "" {
			continue // optional header, blank means unset
		}
		valid := adIDRe.MatchString(s) && strings.Trim(s, ".") != ""
		if k == "api_version" {
			valid = adVersionRe.MatchString(s)
		}
		if !valid {
			return fail("invalid ad destination config: %s", k)
		}
	}
	return nil
}

// pyURL is the part of urllib.parse.urlparse the validators read.
type pyURL struct {
	scheme, hostname string
	username         string
	password         *string
}

func (u pyURL) hasUserinfo() bool {
	return u.username != "" || (u.password != nil && *u.password != "")
}

var errIPv6 = errors.New("Invalid IPv6 URL")

// pyURLParse mirrors urlsplit: leading C0/space stripped, tab/CR/LF removed,
// scheme lower-cased, netloc between // and the first of /?#, userinfo split at
// the last @, bracketed IPv6 hosts validated. Invalid bracketing returns
// "Invalid IPv6 URL" (Python raised ValueError, answered as the 422 detail).
func pyURLParse(raw string) (pyURL, error) {
	url := strings.TrimLeftFunc(raw, func(r rune) bool { return r <= ' ' })
	url = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(url)
	var u pyURL
	if i := strings.IndexByte(url, ':'); i > 0 {
		sch := url[:i]
		valid := sch[0] < 0x80 && (sch[0]|0x20) >= 'a' && (sch[0]|0x20) <= 'z'
		for j := 1; valid && j < len(sch); j++ {
			c := sch[j]
			valid = (c|0x20) >= 'a' && (c|0x20) <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
		}
		if valid {
			u.scheme = strings.ToLower(sch)
			url = url[i+1:]
		}
	}
	if !strings.HasPrefix(url, "//") {
		return u, nil
	}
	netloc := url[2:]
	if i := strings.IndexAny(netloc, "/?#"); i >= 0 {
		netloc = netloc[:i]
	}
	if strings.Contains(netloc, "[") != strings.Contains(netloc, "]") {
		return u, errIPv6
	}
	hostinfo := netloc
	if i := strings.LastIndexByte(netloc, '@'); i >= 0 {
		userinfo := netloc[:i]
		hostinfo = netloc[i+1:]
		user, pw, hasPw := strings.Cut(userinfo, ":")
		u.username = user
		if hasPw {
			u.password = &pw
		}
	}
	if strings.Contains(hostinfo, "[") {
		_, after, _ := strings.Cut(hostinfo, "[")
		host, _, _ := strings.Cut(after, "]")
		if !strings.HasPrefix(host, "v") && !strings.HasPrefix(host, "V") {
			ip, err := netip.ParseAddr(host)
			if err != nil || !ip.Is6() {
				return u, errIPv6
			}
		}
		u.hostname = strings.ToLower(host)
	} else {
		host, _, _ := strings.Cut(hostinfo, ":")
		u.hostname = strings.ToLower(host)
	}
	return u, nil
}
