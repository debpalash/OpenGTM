package collect

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect/scrapers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

// Web is search and page fetching (production: internal/leadweb over the browser service).
type Web interface {
	// Search is _ddg_search: the resilient keyless search; failures are an empty result.
	Search(ctx context.Context, query string, maxResults int) []leadweb.Hit
	// Fetch is StealthClient.fetch(url, tier, timeout): the page text and whether the response
	// was OK.
	Fetch(ctx context.Context, url string, tier int, timeout time.Duration) (text string, ok bool)
}

// Discovery is the browser-service scrapers (production: internal/collect/scrapers).
type Discovery interface {
	Maps(ctx context.Context, query, city string, maxResults int) ([]scrapers.Place, error)
	DecisionMakers(ctx context.Context, company string, maxResults int) ([]scrapers.Person, error)
}

// Sources is the operator source switches and the health gate.
type Sources interface {
	// Enabled is routers.settings.get_source_enabled.
	Enabled(ctx context.Context, id string) bool
	// HealthDisabled is source_health.health_disabled_names (empty unless SOURCE_HEALTH_ENFORCE;
	// fail-open).
	HealthDisabled(ctx context.Context) map[string]bool
}

// ProviderResult is the output of one enrichment provider call (provider-call RPC "output").
type ProviderResult struct {
	Success bool
	Fields  map[string]any
}

// Enrichers are the enrichment steps of the pipeline that are not ported in this package.
type Enrichers interface {
	// Waterfall is email_waterfall.find_email_waterfall for one lead without an email (production:
	// emailverify.Waterfall).
	Waterfall(ctx context.Context, ws string, l *Lead) error
	// Deliverability is email_deliverability.tag_email_confidence (production:
	// emailverify.TagEmailConfidence); ok false = no result (no email / error).
	Deliverability(ctx context.Context, ws string, l *Lead) (keep bool, confidence string, ok bool)
	// Provider runs a named enrichment provider (crosslinked, jobspy, mailscout) on the lead
	// (production: the provider-call RPC signature, served in process once WP-PROVIDERS ports them).
	Provider(ctx context.Context, ws, name string, l *Lead) (ProviderResult, error)
	// HasMX, PickBestEmail, VerifyEmail and Disposable are the email_verify / email_finder probes
	// (production: internal/emailverify). deliverable is nil when unknown.
	HasMX(ctx context.Context, domain string) bool
	PickBestEmail(ctx context.Context, candidates []string) (email, confidence string, deliverable *bool)
	VerifyEmail(ctx context.Context, email string) (deliverable *bool)
	Disposable(email string) bool
}

// Emitter publishes one leadgen progress event for the job's workspace.
type Emitter interface {
	Emit(ctx context.Context, eventType string, data leadprogress.Obj)
}

// Knobs are the env tunables legacy read per run (os.getenv); see config.CollectFrom.
type Knobs struct {
	RegistrySourceCap    int           // REGISTRY_SOURCE_CAP (20)
	DMBudget             int           // DM_DISCOVERY_BUDGET (15)
	EmailWaterfallBudget int           // EMAIL_WATERFALL_BUDGET (15)
	DeliverabilityBudget int           // DELIVERABILITY_BUDGET (20)
	SourceReliability    bool          // SOURCE_RELIABILITY_RANKING (false)
	SearchTimeout        time.Duration // DDG_SEARCH_TIMEOUT (25 s)
	StrategyTimeout      time.Duration // 180 s
}

// DefaultKnobs are the legacy defaults.
func DefaultKnobs() Knobs {
	return Knobs{RegistrySourceCap: 20, DMBudget: 15, EmailWaterfallBudget: 15, DeliverabilityBudget: 20,
		SearchTimeout: 25 * time.Second, StrategyTimeout: 180 * time.Second}
}

// ── ordered values and Python renderings ────────────────────────────────────

// kv is one entry of an insertion-ordered dict.
type kv struct {
	K string
	V any
}

// dict is an insertion-ordered Python dict (json.dumps / repr keep that order).
type dict []kv

func (d dict) get(k string) (any, bool) {
	for _, e := range d {
		if e.K == k {
			return e.V, true
		}
	}
	return nil, false
}

// counter is a dict of string -> int that keeps first-insertion order (reasons, tiers).
type counter struct{ d dict }

func (c *counter) add(k string) {
	for i := range c.d {
		if c.d[i].K == k {
			c.d[i].V = c.d[i].V.(int) + 1
			return
		}
	}
	c.d = append(c.d, kv{k, 1})
}

func (c *counter) dict() dict {
	if c.d == nil {
		return dict{}
	}
	return c.d
}

// pyDumps is json.dumps(v) with the default separators (", ", ": ") and ensure_ascii.
func pyDumps(v any) string {
	var b bytes.Buffer
	dumpValue(&b, v)
	return b.String()
}

func dumpValue(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case float64:
		b.WriteString(pyFloat(x))
	case string:
		asciiString(b, x)
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			asciiString(b, s)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			dumpValue(b, e)
		}
		b.WriteByte(']')
	case dict:
		b.WriteByte('{')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			asciiString(b, e.K)
			b.WriteString(": ")
			dumpValue(b, e.V)
		}
		b.WriteByte('}')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d := make(dict, 0, len(keys))
		for _, k := range keys {
			d = append(d, kv{k, x[k]})
		}
		dumpValue(b, d)
	default:
		asciiString(b, fmt.Sprint(x))
	}
}

func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	s := strconv.FormatFloat(f, 'r', -1, 64)
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	return s
}

func asciiString(b *bytes.Buffer, s string) {
	const hexd = "0123456789abcdef"
	u := func(c rune) {
		b.WriteString(`\u`)
		b.WriteByte(hexd[c>>12&0xf])
		b.WriteByte(hexd[c>>8&0xf])
		b.WriteByte(hexd[c>>4&0xf])
		b.WriteByte(hexd[c&0xf])
	}
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c < 0x20:
			u(c)
		case c < 0x80:
			b.WriteRune(c)
		case c >= 0x10000:
			c -= 0x10000
			u(0xd800 + c>>10)
			u(0xdc00 + c&0x3ff)
		default:
			u(c)
		}
	}
	b.WriteByte('"')
}

// pyRepr is repr() of a str.
func pyRepr(s string) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case byte(r) == q && r < 0x80:
			b.WriteByte('\\')
			b.WriteByte(q)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

// reprCounts is repr() of a str -> int dict.
func reprCounts(d dict) string {
	parts := make([]string, len(d))
	for i, e := range d {
		parts[i] = pyRepr(e.K) + ": " + strconv.Itoa(e.V.(int))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func obj(fields ...any) leadprogress.Obj {
	o := make(leadprogress.Obj, 0, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		o = append(o, leadprogress.Field{Key: fields[i].(string), Val: fields[i+1]})
	}
	return o
}
