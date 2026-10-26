package autosvc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/safecond"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// HTTPError is a REST-style rejection. Routes render {"detail": Detail}.
type HTTPError struct {
	Status int
	Detail any
}

func (e *HTTPError) Error() string { return fmt.Sprintf("http %d: %v", e.Status, e.Detail) }

func herr(status int, format string, a ...any) *HTTPError {
	return &HTTPError{Status: status, Detail: fmt.Sprintf(format, a...)}
}

// SecretChecker reports whether a non-empty per-workspace secret exists
// (secrets.Resolver.Has: workspace-scoped, no global fallback).
type SecretChecker interface {
	Has(ctx context.Context, ws, key string) (bool, error)
}

// txSecretChecker is the optional in-transaction lookup (secrets.Resolver.HasTx):
// ValidateRule holds a tenant transaction, and a second pool connection taken
// for the lookup can starve the pool under concurrent requests.
type txSecretChecker interface {
	HasTx(ctx context.Context, tx pgx.Tx, ws, key string) (bool, error)
}

// Config is the slice of settings the rule service reads.
type Config struct {
	MaxRulesPerWS       int // AUTOMATIONS_MAX_RULES_PER_WS
	MaxActionsPerRule   int // AUTOMATIONS_MAX_ACTIONS_PER_RULE
	AllowLegacyOutreach bool
	// WebhookAllowlist is AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST: exact host or
	// ".suffix"; empty means no create-time host restriction.
	WebhookAllowlist []string
	Secrets          SecretChecker
}

// RuleInput is one rule as submitted. JSON-typed fields are ordered decoded
// values (see ParseJSON): objects are *wbcrud.Obj, numbers json.Number.
type RuleInput struct {
	Name              string
	TriggerType       *string // nil is Python None
	TriggerConfig     any
	Condition         string
	Actions           []any
	ScopeWorkbookIDs  []string
	StopOnError       bool
	MaxSpendUSDPerDay *float64
	MaxActionsPerDay  *int32
	Enabled           bool
	CreatedBy         string // "" stores NULL
}

// Bounds beyond Python (D4): stored JSON stays small.
const maxRuleJSONBytes = 256 << 10

var (
	triggerTypes      = map[string]bool{"on_signal": true, "on_row_changed": true, "on_row_added": true, "on_schedule": true, "on_audience_enter": true, "on_audience_exit": true}
	scheduleIntervals = map[string]bool{"hourly": true, "daily": true, "weekly": true}
)

// pyRepr is repr() of a decoded JSON value.
func pyRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return colgraph.PyStrRepr(t)
	case json.Number:
		return string(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *wbcrud.Obj:
		parts := make([]string, 0, t.Len())
		for _, k := range t.Keys() {
			e, _ := t.Get(k)
			parts = append(parts, colgraph.PyStrRepr(k)+": "+pyRepr(e))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// pyStr is str() of a decoded JSON value.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// asDict returns the object behind `x or {}`: ok is false for a truthy
// non-object (Python's AttributeError on .get).
func asDict(v any) (*wbcrud.Obj, bool) {
	if !truthy(v) {
		return wbcrud.NewObj(), true
	}
	o, ok := v.(*wbcrud.Obj)
	return o, ok
}

// scopedColumns is _scoped_columns: every non-empty string column id across
// the scoped workbooks (all workspace workbooks when the scope is empty).
func scopedColumns(ctx context.Context, tx pgx.Tx, ws string, scope []string) (map[string]bool, error) {
	if scope == nil {
		scope = []string{}
	}
	rows, err := gen.New(tx).AutoSvcScopedColumns(ctx, gen.AutoSvcScopedColumnsParams{WorkspaceID: ws, Ids: scope})
	if err != nil {
		return nil, fmt.Errorf("autosvc: scoped columns: %w", err)
	}
	out := map[string]bool{}
	for _, raw := range rows {
		var cols []map[string]any
		if json.Unmarshal(raw, &cols) != nil {
			continue
		}
		for _, c := range cols {
			if id, ok := c["id"].(string); ok && id != "" {
				out[id] = true
			}
		}
	}
	return out, nil
}

// ValidateRule is _validate_rule with Python's checks in Python's order. The
// returned error is *HTTPError for a rejection, any other error for an
// infrastructure failure. tx must be inside tenancy.InWorkspace for ws.
//
// Python crashes (500) on a non-object action or config, a non-iterable or
// unhashable column_ids and a non-string webhook url; those answer 422 (D4).
func ValidateRule(ctx context.Context, tx pgx.Tx, ws string, r RuleInput, cfg Config) error {
	tt := "None"
	if r.TriggerType != nil {
		tt = *r.TriggerType
	}
	if r.TriggerType == nil || !triggerTypes[tt] {
		return herr(422, "invalid trigger_type '%s'", tt)
	}
	if tt == "on_schedule" {
		d, ok := asDict(r.TriggerConfig)
		if !ok {
			return herr(422, "trigger_config must be an object")
		}
		iv, _ := d.Get("interval")
		if s, isStr := iv.(string); !isStr || !scheduleIntervals[s] {
			return herr(422, "invalid schedule interval '%s'", pyStr(iv))
		}
	}
	maxActions := cfg.MaxActionsPerRule
	if maxActions <= 0 {
		maxActions = 10
	}
	if len(r.Actions) > maxActions {
		return herr(422, "too many actions (max %d)", maxActions)
	}
	if b, err := json.Marshal(r.Actions); err == nil && len(b) > maxRuleJSONBytes {
		return herr(422, "actions too large (max %d bytes)", maxRuleJSONBytes)
	}
	if b, err := json.Marshal(r.TriggerConfig); err == nil && len(b) > maxRuleJSONBytes {
		return herr(422, "trigger_config too large (max %d bytes)", maxRuleJSONBytes)
	}
	if r.MaxSpendUSDPerDay != nil && (math.IsNaN(*r.MaxSpendUSDPerDay) || math.IsInf(*r.MaxSpendUSDPerDay, 0) || *r.MaxSpendUSDPerDay < 0) {
		return herr(422, "max_spend_usd_per_day must be a non-negative finite number")
	}
	if r.MaxActionsPerDay != nil && *r.MaxActionsPerDay < 0 {
		return herr(422, "max_actions_per_day must be non-negative")
	}

	var scoped map[string]bool
	for i, a := range r.Actions {
		action, ok := a.(*wbcrud.Obj)
		if !ok {
			return herr(422, "action %d must be an object", i)
		}
		atype, _ := action.Get("type")
		cv, _ := action.Get("config")
		c, ok := asDict(cv)
		if !ok {
			return herr(422, "action %d: config must be an object", i)
		}
		ts, _ := atype.(string)
		if ts == "sequencer" || ts == "send_email" {
			if !cfg.AllowLegacyOutreach {
				return herr(409, "legacy_outreach_disabled")
			}
			if ts == "sequencer" {
				sv, _ := c.Get("sequence_id")
				seq, _ := sv.(string)
				if colgraph.PyStrip(seq) == "" {
					return herr(422, "sequencer action %d: sequence_id required", i)
				}
			}
			// The outreach store and SMTP live on the Python stack (D2).
			return herr(409, "legacy_outreach_unavailable")
		}
		if ts != "re_enrich" && ts != "push_crm" && ts != "webhook" {
			return herr(422, "invalid action type '%s' at index %d", pyStr(atype), i)
		}
		switch ts {
		case "re_enrich":
			if scoped == nil {
				var err error
				if scoped, err = scopedColumns(ctx, tx, ws, r.ScopeWorkbookIDs); err != nil {
					return err
				}
			}
			ids, _ := c.Get("column_ids")
			if !truthy(ids) {
				return herr(422, "re_enrich action %d: column_ids required", i)
			}
			var entries []any
			switch t := ids.(type) {
			case string:
				for _, ch := range t {
					entries = append(entries, string(ch))
				}
			case []any:
				entries = t
			case *wbcrud.Obj:
				for _, k := range t.Keys() {
					entries = append(entries, k)
				}
			default:
				return herr(422, "re_enrich action %d: column_ids must be a list", i)
			}
			var missing []string
			for _, e := range entries {
				switch t := e.(type) {
				case []any, *wbcrud.Obj:
					return herr(422, "re_enrich action %d: column_ids entries must be strings", i)
				case string:
					if !scoped[t] {
						missing = append(missing, pyRepr(t))
					}
				default:
					missing = append(missing, pyRepr(e))
				}
			}
			if len(missing) > 0 {
				return herr(422, "re_enrich action %d: column_ids not in scoped workbook(s): [%s]", i, strings.Join(missing, ", "))
			}
		case "webhook":
			if err := validateWebhookAction(ctx, tx, ws, i, c, cfg); err != nil {
				return err
			}
		}
	}

	if err := safecond.Validate(r.Condition); err != nil {
		return herr(422, "invalid condition: %s", err.Error())
	}
	return nil
}

func validateWebhookAction(ctx context.Context, tx pgx.Tx, ws string, i int, c *wbcrud.Obj, cfg Config) error {
	uv, _ := c.Get("url")
	var url string
	if truthy(uv) {
		s, ok := uv.(string)
		if !ok {
			return herr(422, "webhook action %d: url must be a string", i)
		}
		url = colgraph.PyStrip(s)
	}
	if url == "" {
		return herr(422, "webhook action %d: url required", i)
	}
	if !strings.Contains(url, "{") {
		p, ok := urlparse(url)
		if !ok {
			return herr(422, "webhook action %d: invalid url", i)
		}
		if p.scheme != "http" && p.scheme != "https" {
			return herr(422, "webhook action %d: only http/https allowed", i)
		}
		if p.username != "" || p.password != "" {
			return herr(422, "webhook action %d: credentials in url not allowed", i)
		}
	}
	if len(cfg.WebhookAllowlist) > 0 {
		// A templated path or query does not exempt a literal host.
		if p, ok := urlparse(url); ok && p.hostname != "" && !strings.Contains(p.hostname, "{") && !AllowlistMatch(p.hostname, cfg.WebhookAllowlist) {
			return herr(422, "webhook action %d: host '%s' not in webhook domain allowlist", i, p.hostname)
		}
	}
	rv, _ := c.Get("header_secret_ref")
	if truthy(rv) {
		found := false
		if ref, ok := rv.(string); ok && cfg.Secrets != nil {
			var err error
			if tc, ok := cfg.Secrets.(txSecretChecker); ok && tx != nil {
				found, err = tc.HasTx(ctx, tx, ws, ref)
			} else {
				found, err = cfg.Secrets.Has(ctx, ws, ref)
			}
			if err != nil {
				return fmt.Errorf("autosvc: secret lookup: %w", err)
			}
		}
		if !found {
			return herr(422, "webhook action %d: header_secret_ref '%s' not found", i, pyStr(rv))
		}
	}
	return nil
}

// AllowlistMatch mirrors the webhook sender's rule: exact host or ".suffix",
// no globs.
func AllowlistMatch(host string, allow []string) bool {
	host = strings.ToLower(host)
	for _, e := range allow {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || strings.ContainsAny(e, "*?[]") {
			continue
		}
		if strings.HasPrefix(e, ".") {
			if len(e) > 1 && strings.HasSuffix(host, e) && len(host) > len(e) {
				return true
			}
			continue
		}
		if host == strings.TrimSuffix(e, ".") {
			return true
		}
	}
	return false
}

// ParsedURL is the part of urllib.parse.urlparse the validators read.
type parsedURL struct {
	scheme, hostname, username, password string
}

// urlparse is urllib.parse.urlparse restricted to scheme, credentials and
// hostname. ok is false where Python raises ValueError (unbalanced brackets).
func urlparse(raw string) (p parsedURL, ok bool) {
	u := strings.TrimLeftFunc(raw, func(r rune) bool { return r <= 0x20 })
	u = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(u)
	if i := strings.IndexByte(u, ':'); i > 0 && isSchemeStart(u[0]) && validSchemeChars(u[:i]) {
		p.scheme = strings.ToLower(u[:i])
		u = u[i+1:]
	}
	netloc := ""
	if strings.HasPrefix(u, "//") {
		rest := u[2:]
		end := len(rest)
		for _, d := range "/?#" {
			if j := strings.IndexRune(rest, d); j >= 0 && j < end {
				end = j
			}
		}
		netloc = rest[:end]
		if (strings.Contains(netloc, "[") && !strings.Contains(netloc, "]")) || (strings.Contains(netloc, "]") && !strings.Contains(netloc, "[")) {
			return p, false
		}
	}
	hostinfo := netloc
	if at := strings.LastIndexByte(netloc, '@'); at >= 0 {
		userinfo := netloc[:at]
		hostinfo = netloc[at+1:]
		if c := strings.IndexByte(userinfo, ':'); c >= 0 {
			p.username, p.password = userinfo[:c], userinfo[c+1:]
		} else {
			p.username = userinfo
		}
	}
	if b := strings.IndexByte(hostinfo, '['); b >= 0 {
		rest := hostinfo[b+1:]
		if e := strings.IndexByte(rest, ']'); e >= 0 {
			rest = rest[:e]
		}
		p.hostname = rest
	} else if c := strings.IndexByte(hostinfo, ':'); c >= 0 {
		p.hostname = hostinfo[:c]
	} else {
		p.hostname = hostinfo
	}
	p.hostname = strings.ToLower(p.hostname)
	if !utf8.ValidString(p.hostname) {
		return p, false
	}
	return p, true
}

func isSchemeStart(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func validSchemeChars(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !(isSchemeStart(b) || (b >= '0' && b <= '9') || b == '+' || b == '-' || b == '.') {
			return false
		}
	}
	return true
}
