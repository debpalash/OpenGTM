package firmo

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

//go:embed data/tech_fingerprints.json
var techFingerprintsJSON []byte

//go:embed data/tech_curated.json
var techCuratedJSON []byte

const (
	techUA       = "Yupcha-TechDetect/1.0 (+https://yupcha.com/bot)"
	techHTMLCap  = 200_000
	techRedirect = 3
	confDirect   = 0.75
	confImplied  = 0.5
)

// matcher reports whether a (lower-cased) subject matches one fingerprint
// pattern. RE2 has no look-ahead, so the single DB pattern that needs one has
// a hand-written matcher.
type matcher func(string) bool

type hdrRx struct {
	name string
	m    matcher
}

type fingerprint struct {
	name, cat string
	implies   []string
	headers   []hdrRx
	html      []matcher
	meta      []hdrRx
	cookies   []string
}

type fpSpec struct {
	Name    string            `json:"name"`
	Cat     *string           `json:"cat"`
	Headers map[string]string `json:"headers"`
	HTML    []string          `json:"html"`
	Meta    map[string]string `json:"meta"`
	Cookies []string          `json:"cookies"`
	Implies []string          `json:"implies"`
}

// drupalMultisite is `sites/(?!default).*/files` (case-insensitive): "sites/"
// not followed by "default", then anything on the same line, then "/files".
func drupalMultisite(s string) bool {
	const pre = "sites/"
	for from := 0; ; {
		i := strings.Index(s[from:], pre)
		if i < 0 {
			return false
		}
		rest := s[from+i+len(pre):]
		if !strings.HasPrefix(rest, "default") {
			line := rest
			if nl := strings.IndexByte(line, '\n'); nl >= 0 {
				line = line[:nl]
			}
			if strings.Contains(line, "/files") {
				return true
			}
		}
		from += i + 1
	}
}

func compilePattern(p string) (matcher, error) {
	if p == `sites/(?!default).*/files` {
		return drupalMultisite, nil
	}
	rx, err := regexp.Compile("(?i)" + p)
	if err != nil {
		return nil, err
	}
	lit := requiredLiteral(p)
	if lit == "" {
		return rx.MatchString, nil
	}
	// Subjects are lower-cased and the pattern is case-insensitive, so a
	// plain substring test on the lower-cased required literal is an exact
	// prefilter that skips the (slow) regexp for almost every page.
	return func(s string) bool { return strings.Contains(s, lit) && rx.MatchString(s) }, nil
}

// requiredLiteral returns the longest literal every match must contain
// (lower-cased), or "" when none is evident.
func requiredLiteral(p string) string {
	re, err := syntax.Parse(p, syntax.Perl|syntax.FoldCase)
	if err != nil {
		return ""
	}
	var best string
	var walk func(*syntax.Regexp)
	walk = func(n *syntax.Regexp) {
		switch n.Op {
		case syntax.OpLiteral:
			if l := strings.ToLower(string(n.Rune)); len(l) > len(best) {
				best = l
			}
		case syntax.OpCapture, syntax.OpConcat:
			for _, sub := range n.Sub {
				walk(sub)
			}
		case syntax.OpPlus:
			walk(n.Sub[0])
		}
	}
	walk(re)
	return best
}

var (
	compiledOnce sync.Once
	compiledFPs  []*fingerprint
)

// orderedSpecs decodes a JSON object into its entries in document order.
func orderedSpecs(raw []byte) ([]fpSpec, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("fingerprints: not an object")
	}
	var out []fpSpec
	idx := map[string]int{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var sp fpSpec
		if err := dec.Decode(&sp); err != nil {
			return nil, err
		}
		sp.Name = kt.(string)
		if i, dup := idx[sp.Name]; dup {
			out[i] = sp
			continue
		}
		idx[sp.Name] = len(out)
		out = append(out, sp)
	}
	return out, nil
}

// techFingerprints is _get_compiled: the bundled DB, with the curated list
// merged over it (curated wins on name, keeps the DB's `implies` when it has
// none), then compiled; a fingerprint with an uncompilable pattern is skipped.
func techFingerprints() []*fingerprint {
	compiledOnce.Do(func() {
		db, err := orderedSpecs(techFingerprintsJSON)
		if err != nil {
			panic(err)
		}
		var curated []fpSpec
		if err := json.Unmarshal(techCuratedJSON, &curated); err != nil {
			panic(err)
		}
		merged := append([]fpSpec(nil), db...)
		idx := map[string]int{}
		for i, sp := range merged {
			idx[sp.Name] = i
		}
		for _, fp := range curated {
			if i, ok := idx[fp.Name]; ok {
				if len(fp.Implies) == 0 {
					fp.Implies = merged[i].Implies
				}
				merged[i] = fp
			} else {
				idx[fp.Name] = len(merged)
				merged = append(merged, fp)
			}
		}
		for _, sp := range merged {
			if f := compileFP(sp); f != nil {
				compiledFPs = append(compiledFPs, f)
			}
		}
	})
	return compiledFPs
}

func compileFP(sp fpSpec) *fingerprint {
	f := &fingerprint{name: sp.Name, cat: "Other", implies: sp.Implies}
	if sp.Cat != nil {
		f.cat = *sp.Cat
	}
	for h, p := range sp.Headers {
		if p == "" {
			p = "."
		}
		m, err := compilePattern(p)
		if err != nil {
			return nil
		}
		f.headers = append(f.headers, hdrRx{strings.ToLower(h), m})
	}
	for _, p := range sp.HTML {
		m, err := compilePattern(p)
		if err != nil {
			return nil
		}
		f.html = append(f.html, m)
	}
	for n, p := range sp.Meta {
		if p == "" {
			p = "."
		}
		m, err := compilePattern(p)
		if err != nil {
			return nil
		}
		f.meta = append(f.meta, hdrRx{n, m})
	}
	for _, c := range sp.Cookies {
		f.cookies = append(f.cookies, strings.ToLower(c))
	}
	return f
}

var techMetaRE = regexp.MustCompile(`(?i)<meta\s[^>]*name=["']?([\w:-]+)["']?[^>]*content=["']([^"']*)["']`)

type detected struct {
	name, category string
	confidence     float64
}

// detectTech is detect_tech_from_response. headers are lower-cased by the
// caller's merge; here they are lower-cased again like Python.
func detectTech(headers map[string]string, htmlText string, cookies []string) []detected {
	htmlLower := strings.ToLower(htmlText)
	hl := make(map[string]string, len(headers))
	for k, v := range headers {
		hl[strings.ToLower(k)] = strings.ToLower(v)
	}
	cl := make(map[string]bool, len(cookies))
	for _, c := range cookies {
		cl[strings.ToLower(c)] = true
	}
	metaTags := map[string]string{}
	for _, m := range techMetaRE.FindAllStringSubmatch(htmlLower, -1) {
		k := strings.ToLower(m[1])
		if _, ok := metaTags[k]; !ok {
			metaTags[k] = m[2]
		}
	}
	fps := techFingerprints()
	var names []string
	cats := map[string]string{}
	direct := map[string]bool{}
	for _, fp := range fps {
		if _, ok := cats[fp.name]; ok {
			continue
		}
		hit := false
		for _, h := range fp.headers {
			if v, ok := hl[h.name]; ok && h.m(v) {
				hit = true
				break
			}
		}
		if !hit {
			for _, m := range fp.html {
				if m(htmlLower) {
					hit = true
					break
				}
			}
		}
		if !hit {
			for _, mt := range fp.meta {
				if c, ok := metaTags[strings.ToLower(mt.name)]; ok && mt.m(c) {
					hit = true
					break
				}
			}
		}
		if !hit {
			for _, c := range fp.cookies {
				if cl[c] {
					hit = true
					break
				}
			}
		}
		if hit {
			names = append(names, fp.name)
			cats[fp.name] = fp.cat
			direct[fp.name] = true
		}
	}
	byName := make(map[string]*fingerprint, len(fps))
	for _, fp := range fps {
		byName[fp.name] = fp
	}
	queue := append([]string(nil), names...)
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		fp := byName[cur]
		if fp == nil {
			continue
		}
		for _, imp := range fp.implies {
			imp, _, _ = strings.Cut(imp, `\;`)
			if _, seen := cats[imp]; imp != "" && !seen {
				cat := "Other"
				if ifp := byName[imp]; ifp != nil {
					cat = ifp.cat
				}
				names = append(names, imp)
				cats[imp] = cat
				queue = append(queue, imp)
			}
		}
	}
	out := make([]detected, len(names))
	for i, n := range names {
		c := confImplied
		if direct[n] {
			c = confDirect
		}
		out[i] = detected{n, cats[n], c}
	}
	return out
}

// structuredFields is _structured_to_fields.
func structuredFields(techs []detected) *fields {
	names := make([]string, len(techs))
	st := make([]jo, len(techs))
	for i, t := range techs {
		names[i] = t.name
		st[i] = jo{{"name", t.name}, {"category", t.category}, {"source", "website"}, {"confidence", t.confidence}}
	}
	f := newFields()
	f.Set("technologies", strings.Join(names, ", "))
	f.Set("technographics", dumpsCompact(st))
	return f
}

func settingBool(in providers.Input, key string, def bool) bool {
	v, ok := in.Settings[key]
	if !ok {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off", "":
			return false
		}
	}
	return def
}

// techStack ports providers/tech_stack_provider.py (TechStackProvider). The
// per-domain 90-day cache lives in the workspace-scoped enrichment cache
// (Task 11/18), not here. The homepage fetch is off unless
// Settings["TECH_STACK_WEBSITE_FETCH_ENABLED"] is true (global operator
// config, default off exactly like Python); TECH_STACK_RESPECT_ROBOTS
// defaults to true; TLS verification is always on.
type techStack struct{}

func (techStack) Name() string { return "tech_stack" }

func (p techStack) Enrich(ctx context.Context, c *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	website := lead(in, "website")
	if website == "" {
		return fail("No website URL available"), nil
	}
	if !settingBool(in, "TECH_STACK_WEBSITE_FETCH_ENABLED", false) {
		return fail("website_fetch_disabled"), nil
	}
	out, err := p.run(ctx, c, website, in)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return providers.Output{}, cerr
		}
		var be *blockedErr
		if errors.As(err, &be) {
			return fail("blocked_url: " + providers.TruncRunes(be.msg, 80)), nil
		}
		return fail(providers.TruncRunes(err.Error(), 200)), nil
	}
	return out, nil
}

func (p techStack) run(ctx context.Context, c *http.Client, website string, in providers.Input) (providers.Output, error) {
	u := normalizeWebsiteURL(website)
	if settingBool(in, "TECH_STACK_RESPECT_ROBOTS", true) {
		ok, err := p.robotsAllow(ctx, c, u)
		if err != nil {
			return providers.Output{}, err
		}
		if !ok {
			return fail("robots_disallowed"), nil
		}
	}
	headers, text, cookies, err := safeGet(ctx, c, u)
	if err != nil {
		return providers.Output{}, err
	}
	techs := detectTech(headers, text, cookies)
	if len(techs) == 0 {
		return fail("No technologies detected"), nil
	}
	return success(in, structuredFields(techs), confDirect), nil
}

// robotsAllow is _robots_allows: fail-open on every error except a cancelled
// context.
func (p techStack) robotsAllow(ctx context.Context, c *http.Client, base string) (bool, error) {
	pu, err := pySplitURL(base)
	if err != nil {
		return true, nil
	}
	robotsURL := pu.scheme + "://" + pu.netloc + "/robots.txt"
	if checkURL(robotsURL) != nil {
		return true, nil
	}
	_, body, _, err := safeGet(ctx, c, robotsURL)
	if cerr := ctx.Err(); cerr != nil {
		return false, cerr
	}
	if err != nil {
		return true, nil
	}
	return parseRobots(body).canFetchRoot(techUA), nil
}

// safeGet is _safe_get: every hop (initial and each redirect) is checked
// before the request; redirects are followed manually, at most techRedirect.
func safeGet(ctx context.Context, c *http.Client, u string) (headers map[string]string, text string, cookies []string, err error) {
	resp, err := guardedGet(ctx, c, u, techRedirect, [][2]string{
		{"User-Agent", techUA}, {"Accept", "text/html,application/xhtml+xml"}, {"Accept-Language", "en-US,en;q=0.9"}}, true)
	if err != nil {
		return nil, "", nil, err
	}
	headers = make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		headers[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if name, _, ok := strings.Cut(sc, "="); ok {
			cookies = append(cookies, providers.PyStrip(name))
		}
	}
	return headers, truncChars(decodeText(resp.Body), techHTMLCap), cookies, nil
}
