package firmo

import (
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// A port of Python 3.13's urllib.robotparser.RobotFileParser.parse/can_fetch,
// specialised to the single URL the tech stack provider checks: a site root
// ("https://host", empty path), which Python rewrites to "/".

type robotRule struct {
	path      string
	allowance bool
}

type robotEntry struct {
	agents []string
	rules  []robotRule
}

type robotsTxt struct {
	entries []*robotEntry
	deflt   *robotEntry
}

func (r *robotsTxt) add(e *robotEntry) {
	for _, a := range e.agents {
		if a == "*" {
			if r.deflt == nil {
				r.deflt = e
			}
			return
		}
	}
	r.entries = append(r.entries, e)
}

// pySplitLines is str.splitlines().
func pySplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			i += w
			start = i
		case '\r':
			out = append(out, s[start:i])
			i += w
			if i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
		default:
			i += w
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// pyUnquote is urllib.parse.unquote (UTF-8, errors="replace").
func pyUnquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if h, ok := unhex2(s[i+1], s[i+2]); ok {
				b = append(b, h)
				i += 2
				continue
			}
		}
		b = append(b, s[i])
	}
	return strings.ToValidUTF8(string(b), "\ufffd")
}

func unhex2(a, b byte) (byte, bool) {
	hv := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	x, ok1 := hv(a)
	y, ok2 := hv(b)
	return x<<4 | y, ok1 && ok2
}

func parseRobots(body string) *robotsTxt {
	r := &robotsTxt{}
	state := 0
	entry := &robotEntry{}
	for _, line := range pySplitLines(body) {
		if line == "" {
			if state == 1 {
				entry = &robotEntry{}
				state = 0
			} else if state == 2 {
				r.add(entry)
				entry = &robotEntry{}
				state = 0
			}
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = providers.PyStrip(line)
		if line == "" {
			continue
		}
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		k = strings.ToLower(providers.PyStrip(k))
		v = pyUnquote(providers.PyStrip(v))
		switch k {
		case "user-agent":
			if state == 2 {
				r.add(entry)
				entry = &robotEntry{}
			}
			entry.agents = append(entry.agents, v)
			state = 1
		case "disallow":
			if state != 0 {
				entry.rules = append(entry.rules, newRule(v, false))
				state = 2
			}
		case "allow":
			if state != 0 {
				entry.rules = append(entry.rules, newRule(v, true))
				state = 2
			}
		case "crawl-delay", "request-rate":
			if state != 0 {
				state = 2
			}
		}
	}
	if state == 2 {
		r.add(entry)
	}
	return r
}

// newRule is RuleLine.__init__ reduced to what decides a match against "/":
// urlunparse(urlparse(path)) drops an empty query/fragment/params marker, and
// quote() only changes characters that can never be a prefix of "/".
func newRule(path string, allowance bool) robotRule {
	if path == "" && !allowance {
		allowance = true
	}
	return robotRule{path: normalizeRulePath(path), allowance: allowance}
}

func normalizeRulePath(p string) string {
	// urlparse reads "//host/x" as netloc "host"; with an empty netloc
	// urlunparse drops the "//" ("//" -> "", "///x" -> "/x").
	if strings.HasPrefix(p, "//") {
		if end := strings.IndexAny(p[2:], "/?#"); end == 0 || end < 0 && len(p) == 2 {
			p = p[2:]
		}
	}
	frag := ""
	if i := strings.Index(p, "#"); i >= 0 {
		p, frag = p[:i], p[i+1:]
	}
	query := ""
	if i := strings.Index(p, "?"); i >= 0 {
		p, query = p[:i], p[i+1:]
	}
	// params: ";" in the last path segment
	params := ""
	last := strings.LastIndex(p, "/")
	if i := strings.Index(p[last+1:], ";"); i >= 0 {
		params = p[last+1+i+1:]
		p = p[:last+1+i]
	}
	if params != "" {
		p += ";" + params
	}
	if query != "" {
		p += "?" + query
	}
	if frag != "" {
		p += "#" + frag
	}
	return quotePath(p)
}

// quotePath is urllib.parse.quote(s) (safe="/").
func quotePath(s string) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~/", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexd[c>>4])
		b.WriteByte(hexd[c&15])
	}
	return b.String()
}

func (e *robotEntry) appliesTo(ua string) bool {
	ua, _, _ = strings.Cut(ua, "/")
	ua = strings.ToLower(ua)
	for _, a := range e.agents {
		if a == "*" {
			return true
		}
		if strings.Contains(ua, strings.ToLower(a)) {
			return true
		}
	}
	return false
}

func (e *robotEntry) allowance(filename string) bool {
	for _, r := range e.rules {
		if r.path == "*" || strings.HasPrefix(filename, r.path) {
			return r.allowance
		}
	}
	return true
}

// canFetchRoot is can_fetch(ua, "https://host") (path "/").
func (r *robotsTxt) canFetchRoot(ua string) bool {
	for _, e := range r.entries {
		if e.appliesTo(ua) {
			return e.allowance("/")
		}
	}
	if r.deflt != nil {
		return r.deflt.allowance("/")
	}
	return true
}
