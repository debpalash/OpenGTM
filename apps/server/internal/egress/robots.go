package egress

import (
	"bufio"
	"bytes"
	"container/list"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RobotsToken is the product token OpenGTM matches in robots.txt groups.
const RobotsToken = "opengtm"

// robotsMaxBytes is the parse limit (RFC 9309 requires at least 500 KiB).
const robotsMaxBytes = 512 << 10

type robotsRule struct {
	allow   bool
	pattern string
}

// Robots is a parsed robots.txt, reduced to the group that applies to
// OpenGTM: the groups naming "OpenGTM" if any, else the "*" groups.
type Robots struct {
	rules      []robotsRule
	crawlDelay time.Duration
	// mode: allow everything / disallow everything / use rules.
	allowAll    bool
	disallowAll bool
}

// AllowAllRobots allows every path (missing robots.txt).
func AllowAllRobots() *Robots { return &Robots{allowAll: true} }

// DisallowAllRobots refuses every path (unreachable robots.txt).
func DisallowAllRobots() *Robots { return &Robots{disallowAll: true} }

// ParseRobots parses robots.txt content per RFC 9309: user-agent lines
// start a group (consecutive ones share it), groups for the same agent are
// merged, the most specific matching agent wins over "*".
func ParseRobots(body []byte) *Robots {
	if len(body) > robotsMaxBytes {
		body = body[:robotsMaxBytes]
	}
	type group struct {
		agents []string
		rules  []robotsRule
		delay  time.Duration
		delayS bool
	}
	var groups []*group
	var cur *group
	lastWasAgent := false
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), robotsMaxBytes)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "user-agent":
			if !lastWasAgent || cur == nil {
				cur = &group{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, strings.ToLower(val))
			lastWasAgent = true
		case "allow", "disallow":
			lastWasAgent = false
			if cur == nil || val == "" {
				continue // empty disallow means "allow all": no rule
			}
			cur.rules = append(cur.rules, robotsRule{allow: key == "allow", pattern: val})
		case "crawl-delay":
			lastWasAgent = false
			if cur == nil {
				continue
			}
			if f, err := strconv.ParseFloat(val, 64); err == nil && f >= 0 {
				cur.delay = time.Duration(f * float64(time.Second))
				cur.delayS = true
			}
		default:
			// sitemap and unknown keys do not end a group of agents
		}
	}
	pick := func(match func(agent string) bool) *Robots {
		r := &Robots{}
		found := false
		for _, g := range groups {
			for _, a := range g.agents {
				if match(a) {
					found = true
					r.rules = append(r.rules, g.rules...)
					if g.delayS && g.delay > r.crawlDelay {
						r.crawlDelay = g.delay
					}
					break
				}
			}
		}
		if !found {
			return nil
		}
		return r
	}
	if r := pick(func(a string) bool {
		a = strings.TrimSpace(a)
		if i := strings.IndexByte(a, '/'); i >= 0 {
			a = a[:i] // tolerate "OpenGTM/1.0"
		}
		return a == RobotsToken
	}); r != nil {
		return r
	}
	if r := pick(func(a string) bool { return a == "*" }); r != nil {
		return r
	}
	return AllowAllRobots()
}

// CrawlDelay returns the Crawl-delay for the chosen group (0 when unset).
func (r *Robots) CrawlDelay() time.Duration { return r.crawlDelay }

// Allowed applies longest-match semantics (allow wins ties) to the URL's
// path and query. "/robots.txt" is always allowed.
func (r *Robots) Allowed(u *url.URL) bool {
	if r.allowAll {
		return true
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if p == "/robots.txt" {
		return true
	}
	if r.disallowAll {
		return false
	}
	target := p
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	bestLen := -1
	allowed := true
	for _, rule := range r.rules {
		if !robotsMatch(rule.pattern, target) {
			continue
		}
		n := len(rule.pattern)
		if n > bestLen || (n == bestLen && rule.allow) {
			bestLen = n
			allowed = rule.allow
		}
	}
	return allowed
}

// robotsMatch matches a robots path pattern with '*' wildcards and a
// trailing '$' end anchor against a path (prefix match otherwise).
func robotsMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	pattern = normalizeEscapes(pattern)
	path = normalizeEscapes(path)
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	pos := len(parts[0])
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if i == len(parts)-1 && anchored {
			return len(path)-pos >= len(part) && strings.HasSuffix(path, part)
		}
		idx := strings.Index(path[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	if anchored {
		return pos == len(path)
	}
	return true
}

// normalizeEscapes upper-cases percent escapes so "%2f" and "%2F" compare equal.
func normalizeEscapes(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' {
			b[i+1] = upperHex(b[i+1])
			b[i+2] = upperHex(b[i+2])
		}
	}
	return string(b)
}

func upperHex(c byte) byte {
	if c >= 'a' && c <= 'f' {
		return c - 32
	}
	return c
}

// robotsCache is a bounded LRU of parsed robots.txt per origin.
type robotsCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
}

type robotsEntry struct {
	origin  string
	robots  *Robots
	expires time.Time
}

func newRobotsCache(max int) *robotsCache {
	if max <= 0 {
		max = 1024
	}
	return &robotsCache{max: max, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *robotsCache) get(origin string, now time.Time) (*Robots, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[origin]
	if !ok {
		return nil, false
	}
	e := el.Value.(*robotsEntry)
	if now.After(e.expires) {
		c.ll.Remove(el)
		delete(c.items, origin)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return e.robots, true
}

func (c *robotsCache) put(origin string, r *Robots, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[origin]; ok {
		el.Value = &robotsEntry{origin, r, expires}
		c.ll.MoveToFront(el)
		return
	}
	c.items[origin] = c.ll.PushFront(&robotsEntry{origin, r, expires})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*robotsEntry).origin)
	}
}

func (c *robotsCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
