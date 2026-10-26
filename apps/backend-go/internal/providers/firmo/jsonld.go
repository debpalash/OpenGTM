package firmo

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	jsonldUA     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	jsonldCap    = 300_000 // characters of page text parsed
	maxRedirects = 6
)

var jsonldOrgTypes = map[string]bool{
	"organization": true, "corporation": true, "localbusiness": true, "company": true, "ngo": true,
	"educationalorganization": true, "governmentorganization": true, "professionalservice": true,
	"store": true, "itservice": true, "softwareapplication": true,
}

var socialMap = []struct{ dom, key string }{
	{"linkedin.com", "linkedin_url"}, {"twitter.com", "twitter_url"}, {"x.com", "twitter_url"}, {"facebook.com", "facebook_url"},
}

var (
	yearRE   = regexp.MustCompile(`(\p{Nd}{4})`)
	mailtoRE = regexp.MustCompile(`(?i)^mailto:`)
)

// jsonldFirmo ports providers/jsonld_firmographics.py.
type jsonldFirmo struct{}

func (jsonldFirmo) Name() string { return "jsonld_firmographics" }

func (p jsonldFirmo) Enrich(ctx context.Context, c *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	website := lead(in, "website")
	if website == "" {
		return fail("no_website"), nil
	}
	out, err := p.run(ctx, c, website, in)
	return finish(ctx, out, err, 200)
}

func (p jsonldFirmo) run(ctx context.Context, c *http.Client, website string, in providers.Input) (providers.Output, error) {
	resp, err := guardedGet(ctx, c, normalizeWebsiteURL(website), maxRedirects, [][2]string{
		{"User-Agent", jsonldUA}, {"Accept", "text/html,application/xhtml+xml"}}, false)
	if err != nil {
		return providers.Output{}, err
	}
	text := truncChars(decodeText(resp.Body), jsonldCap)
	fields := extractFirmographics(text)
	if lead(in, "company") != "" {
		fields.remove("company")
	}
	if v, ok := fields.get("founded_year"); ok {
		fields.set("founding_year", v)
	}
	if len(fields) == 0 {
		return fail("no_structured_data"), nil
	}
	f := newFields()
	for _, e := range fields {
		f.Set(e.k, e.v)
	}
	return success(in, f, 0.85), nil
}

// ordered string map (a Python dict of str -> str).
type ofields []kv

func (o *ofields) set(k string, v any) {
	for i := range *o {
		if (*o)[i].k == k {
			(*o)[i].v = v
			return
		}
	}
	*o = append(*o, kv{k, v})
}
func (o ofields) get(k string) (any, bool) {
	for _, e := range o {
		if e.k == k {
			return e.v, true
		}
	}
	return nil, false
}
func (o ofields) has(k string) bool { _, ok := o.get(k); return ok }
func (o *ofields) remove(k string) {
	for i := range *o {
		if (*o)[i].k == k {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

// decodeText is httpx's resp.text for a UTF-8 body: invalid bytes become U+FFFD.
func decodeText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return string([]rune(string(b)))
}

// truncChars is s[:n] by characters.
func truncChars(s string, n int) string { return providers.TruncRunes(s, n) }

// ── guarded GET (core.url_guard.guarded_get) ──────────────────────────────

// guardedGet GETs u validating every hop with the egress URL check and
// following redirects manually, as Python's guarded_get does. The client's
// dial-time check still applies to every connection. A 3xx without Location is
// returned as is.
func guardedGet(ctx context.Context, c *http.Client, u string, maxHops int, header [][2]string, strict bool) (providers.Response, error) {
	cur := u
	for hop := 0; hop <= maxHops; hop++ {
		if err := checkURL(cur); err != nil {
			return providers.Response{}, err
		}
		resp, err := providers.Do(ctx, c, providers.Req{Method: http.MethodGet, URL: cur, Header: header})
		if err != nil {
			return providers.Response{}, unwrapBlocked(err)
		}
		switch resp.Status {
		case 301, 302, 303, 307, 308:
			loc := resp.Header.Get("Location")
			if loc == "" {
				if strict { // tech stack: `break`, then "too many redirects"
					return providers.Response{}, &blockedErr{"too many redirects"}
				}
				return resp, nil
			}
			next, err := joinURL(cur, loc)
			if err != nil {
				return providers.Response{}, err
			}
			cur = next
			continue
		}
		return resp, nil
	}
	return providers.Response{}, &blockedErr{"too many redirects"}
}

// blockedErr is Python's BlockedUrlError.
type blockedErr struct{ msg string }

func (e *blockedErr) Error() string { return e.msg }

// checkURL is check_url(allow_http=True): a BlockedUrlError carries the Python
// message.
func checkURL(u string) error {
	if err := egress.CheckURL(u, true); err != nil {
		var be *egress.BlockedError
		if errors.As(err, &be) {
			return &blockedErr{be.Reason}
		}
		return err
	}
	return nil
}

// unwrapBlocked turns a dial-time egress rejection (reported by the transport
// as text) into the BlockedUrlError message Python raises.
func unwrapBlocked(err error) error {
	if msg, ok := strings.CutPrefix(err.Error(), "egress: blocked: "); ok {
		return &blockedErr{msg}
	}
	if i := strings.Index(err.Error(), "egress: blocked: "); i >= 0 {
		return &blockedErr{err.Error()[i+len("egress: blocked: "):]}
	}
	return err
}

func joinURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

// ── extraction ────────────────────────────────────────────────────────────

func str(v any) (string, bool) { s, ok := v.(string); return s, ok }

func nonBlankString(v any) (string, bool) {
	s, ok := str(v)
	if !ok || providers.PyStrip(s) == "" {
		return "", false
	}
	return providers.PyStrip(s), true
}

func typesOf(node map[string]any) []string {
	t, present := node["@type"]
	if !present {
		return []string{""}
	}
	if l, ok := t.([]any); ok {
		out := make([]string, len(l))
		for i, x := range l {
			out[i] = strings.ToLower(pyStr(x))
		}
		return out
	}
	return []string{strings.ToLower(pyStr(t))}
}

// iterJSONLD yields the JSON objects of the ld+json blocks in Python's order
// (LIFO stack) until yield returns false.
func iterJSONLD(blocks []string, yield func(map[string]any) bool) {
	for _, block := range blocks {
		raw := providers.PyStrip(block)
		if raw == "" {
			continue
		}
		data, err := parseLenientJSON(raw)
		if err != nil {
			continue
		}
		stack := []any{data}
		for len(stack) > 0 {
			node := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			switch t := node.(type) {
			case []any:
				stack = append(stack, t...)
			case map[string]any:
				if g, ok := t["@graph"].([]any); ok {
					stack = append(stack, g...)
				}
				if !yield(t) {
					return
				}
			}
		}
	}
}

var (
	nanRE = regexp.MustCompile(`-?Infinity|NaN`)
)

// parseLenientJSON is json.loads: like ParseJSON but also accepting the NaN /
// Infinity / -Infinity literals Python allows.
func parseLenientJSON(raw string) (any, error) {
	v, err := providers.ParseJSON([]byte(raw))
	if err == nil || !nanRE.MatchString(raw) {
		return v, err
	}
	// Replace the literals outside strings, then retry.
	var b strings.Builder
	inStr, esc := false, false
	for i := 0; i < len(raw); {
		ch := raw[i]
		if inStr {
			b.WriteByte(ch)
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			i++
			continue
		}
		switch {
		case ch == '"':
			inStr = true
			b.WriteByte(ch)
			i++
		case strings.HasPrefix(raw[i:], "-Infinity"):
			b.WriteString("-1e999")
			i += len("-Infinity")
		case strings.HasPrefix(raw[i:], "Infinity"):
			b.WriteString("1e999")
			i += len("Infinity")
		case strings.HasPrefix(raw[i:], "NaN"):
			b.WriteString("null")
			i += len("NaN")
		default:
			b.WriteByte(ch)
			i++
		}
	}
	return providers.ParseJSON([]byte(b.String()))
}

func flattenAddress(addr any) string {
	switch t := addr.(type) {
	case string:
		return providers.PyStrip(t)
	case []any:
		if len(t) > 0 {
			return flattenAddress(t[0])
		}
	case map[string]any:
		var parts []string
		for _, k := range []string{"streetAddress", "addressLocality", "addressRegion", "postalCode"} {
			if p := t[k]; providers.Truthy(p) {
				parts = append(parts, providers.PyStrip(pyStr(p)))
			}
		}
		if c, ok := t["addressCountry"].(string); ok && c != "" {
			parts = append(parts, providers.PyStrip(c))
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func locality(addr any) (city, state string, ok bool) {
	if l, isList := addr.([]any); isList && len(l) > 0 {
		addr = l[0]
	}
	m, isMap := addr.(map[string]any)
	if !isMap {
		return "", "", false
	}
	s := func(k string) string {
		v := m[k]
		if !providers.Truthy(v) {
			return ""
		}
		return providers.PyStrip(pyStr(v))
	}
	return s("addressLocality"), s("addressRegion"), true
}

func yearOf(v any) string {
	s := ""
	if providers.Truthy(v) {
		s = pyStr(v)
	}
	return yearRE.FindString(s)
}

// extractFirmographics is extract_firmographics.
func extractFirmographics(htmlText string) ofields {
	var fields ofields
	var socials ofields
	meta := parseHTMLMeta(htmlText)

	var org map[string]any
	iterJSONLD(meta.jsonld, func(node map[string]any) bool {
		for _, t := range typesOf(node) {
			if jsonldOrgTypes[t] {
				org = node
				return false
			}
		}
		return true
	})
	if len(org) > 0 {
		name := org["name"]
		if !providers.Truthy(name) {
			name = org["legalName"]
		}
		if s, ok := nonBlankString(name); ok {
			fields.set("company", s)
		}
		for _, k := range []string{"telephone", "phone"} {
			if s, ok := nonBlankString(org[k]); ok {
				fields.set("phone", s)
				break
			}
		}
		cp := org["contactPoint"]
		if !fields.has("phone") {
			var cp0 any
			switch t := cp.(type) {
			case []any:
				if len(t) > 0 {
					cp0 = t[0]
				} else {
					cp0 = t
				}
			case map[string]any:
				cp0 = t
			}
			if m, ok := cp0.(map[string]any); ok && providers.Truthy(m["telephone"]) {
				fields.set("phone", providers.PyStrip(pyStr(m["telephone"])))
			}
		}
		if email, ok := org["email"].(string); ok && strings.Contains(email, "@") {
			fields.set("email", mailtoRE.ReplaceAllString(providers.PyStrip(email), ""))
		}
		if a := flattenAddress(org["address"]); a != "" {
			fields.set("address", a)
		}
		if city, state, ok := locality(org["address"]); ok {
			if city != "" {
				fields.set("city", city)
			}
			if state != "" {
				fields.set("state", state)
			}
		}
		fd := org["foundingDate"]
		if !providers.Truthy(fd) {
			fd = org["foundingdate"]
		}
		if fy := yearOf(fd); fy != "" {
			fields.set("founded_year", fy)
		}
		if d, ok := nonBlankString(org["description"]); ok {
			fields.set("description", truncChars(html.UnescapeString(d), 500))
		}
		same := org["sameAs"]
		if s, ok := same.(string); ok {
			same = []any{s}
		}
		if l, ok := same.([]any); ok {
			for _, u := range l {
				us, ok := u.(string)
				if !ok {
					continue
				}
				us = providers.PyStrip(us)
				pu, err := pySplitURL(us)
				if err != nil {
					continue
				}
				host := strings.TrimRight(pu.hostname(), ".")
				for _, sm := range socialMap {
					if (host == sm.dom || strings.HasSuffix(host, "."+sm.dom)) && !socials.has(sm.key) {
						socials.set(sm.key, us)
					}
				}
			}
		}
	}
	if !fields.has("company") && meta.og["site_name"] != "" {
		fields.set("company", providers.PyStrip(meta.og["site_name"]))
	}
	if !fields.has("description") && meta.og["description"] != "" {
		fields.set("description", truncChars(providers.PyStrip(meta.og["description"]), 500))
	}
	for _, e := range socials {
		fields.set(e.k, e.v)
	}
	var out ofields
	for _, e := range fields {
		if s, ok := e.v.(string); ok && s == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}
