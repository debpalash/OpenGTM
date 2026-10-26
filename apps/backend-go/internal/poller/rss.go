package poller

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// FeedUA is the poller User-Agent (rss._UA).
const FeedUA = "Yupcha-Poller (+https://yupcha.com)"

const (
	feedAccept      = "application/rss+xml, application/atom+xml, application/xml, text/xml"
	feedTimeout     = 10 * time.Second
	feedHostSpacing = time.Second
	// FeedMaxBody bounds a feed body (D4 hardening; Python reads it all).
	FeedMaxBody = 1 << 20
	// maxFeedNodes bounds the element tree built while parsing (a 1 MiB body of
	// "<a/>" would otherwise build ~250k nodes).
	maxFeedNodes = 100000
	// maxFeedDepth bounds element nesting; maxNodeText the text kept per node
	// (summary is cut to 2000 anyway); maxEntityExpansion the total bytes DTD
	// entities may expand to in one feed.
	maxFeedDepth       = 256
	maxNodeText        = 64 << 10
	maxEntityExpansion = 2 << 20
)

// FeedEntry is one parsed RSS item / Atom entry.
type FeedEntry struct {
	GUID           string
	Title          string
	Link           string
	Summary        string
	PublishedEpoch float64
}

// FeedResult is rss.FeedResult. Error is a string (Python never raises).
type FeedResult struct {
	Status       int
	NotModified  bool
	Entries      []FeedEntry
	ETag         *string
	LastModified *string
	Error        string
}

// ── XML tree (ElementTree subset: tag, attrs, text before the first child) ──

type xnode struct {
	local string
	attrs map[string]string // un-prefixed attributes only (ET keys prefixed ones as {ns}name)
	text  string
	tb    *strings.Builder
	kids  []*xnode
}

var entityDeclRE = regexp.MustCompile(`<!ENTITY\s+([A-Za-z_][A-Za-z0-9_.-]*)\s+(?:'([^']*)'|"([^"]*)")`)

// parseTree is ET.fromstring: any well-formedness error rejects the whole
// document. Internal DTD entities are expanded one level as text (never
// recursively, so there is no entity-expansion blow-up); external entities are
// not resolved.
func parseTree(src []byte) (*xnode, error) {
	src = bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	src = []byte(strings.ToValidUTF8(string(src), "�"))
	dec := xml.NewDecoder(bytes.NewReader(src))
	dec.Entity = map[string]string{}
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	var stack []*xnode
	var root *xnode
	nodes := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.Directive:
			var budget int64
			for _, m := range entityDeclRE.FindAllSubmatch(t, 64) {
				val := string(m[2])
				if len(m[3]) > 0 {
					val = string(m[3])
				}
				if len(val) <= 4096 {
					dec.Entity[string(m[1])] = val
					// the decoder expands references before we see the token, so
					// bound the total expansion up front from the raw reference count
					budget += int64(bytes.Count(src, []byte("&"+string(m[1])+";"))) * int64(len(val))
					if budget > maxEntityExpansion {
						return nil, errors.New("entity expansion too large")
					}
				}
			}
		case xml.StartElement:
			nodes++
			if nodes > maxFeedNodes {
				return nil, errors.New("feed too large")
			}
			n := &xnode{local: t.Name.Local}
			for _, a := range t.Attr {
				if a.Name.Space == "" {
					if n.attrs == nil {
						n.attrs = map[string]string{}
					}
					n.attrs[a.Name.Local] = a.Value
				}
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("junk after document element")
				}
				root = n
			} else {
				p := stack[len(stack)-1]
				p.kids = append(p.kids, n)
			}
			stack = append(stack, n)
			if len(stack) > maxFeedDepth {
				return nil, errors.New("feed nested too deeply")
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 {
				if len(bytes.TrimSpace(t)) > 0 {
					return nil, errors.New("text outside root")
				}
				continue
			}
			p := stack[len(stack)-1]
			if len(p.kids) == 0 && (p.tb == nil || p.tb.Len() < maxNodeText) { // ET .text is only the text before the first child
				if p.tb == nil {
					p.tb = &strings.Builder{}
				}
				room := maxNodeText - p.tb.Len()
				if len(t) > room {
					t = t[:room]
				}
				p.tb.Write(t)
			}
		}
	}
	if root != nil {
		root.walk(func(n *xnode) bool {
			if n.tb != nil {
				n.text = strings.ToValidUTF8(n.tb.String(), "\ufffd")
				n.tb = nil
			}
			return true
		})
	}
	if root == nil || len(stack) != 0 {
		return nil, errors.New("no element found")
	}
	return root, nil
}

func (n *xnode) walk(fn func(*xnode) bool) bool {
	if !fn(n) {
		return false
	}
	for _, k := range n.kids {
		if !k.walk(fn) {
			return false
		}
	}
	return true
}

// ParseFeed is rss.parse_feed: RSS 2.0 <item> and Atom <entry> in document
// order, localnames only, at most maxEntries (the Python check runs after the
// append, so maxEntries <= 0 still yields one). A malformed document is an
// error and no entries (Python returns []).
func ParseFeed(src []byte, maxEntries int) ([]FeedEntry, error) {
	root, err := parseTree(src)
	if err != nil {
		return nil, err
	}
	entries := []FeedEntry{}
	root.walk(func(node *xnode) bool {
		if node.local != "item" && node.local != "entry" {
			return true
		}
		var title, guid, link, summary, published string
		for _, c := range node.kids {
			text := providers.PyStrip(c.text)
			switch c.local {
			case "title":
				title = text
			case "guid", "id":
				guid = text
			case "link":
				switch {
				case text != "":
					link = text
				case c.attrs["href"] != "":
					link = c.attrs["href"]
				}
			case "description", "summary", "content":
				if summary == "" {
					summary = text
				}
			case "pubDate", "published", "updated", "date":
				if published == "" {
					published = text
				}
			}
		}
		natural := guid
		if natural == "" {
			natural = link
		}
		if natural == "" {
			natural = title
		}
		if natural == "" {
			return true
		}
		entries = append(entries, FeedEntry{
			GUID: natural, Title: title, Link: link, Summary: providers.TruncRunes(summary, 2000),
			PublishedEpoch: parseEpoch(published),
		})
		return len(entries) < maxEntries
	})
	return entries, nil
}

// ── _parse_epoch ────────────────────────────────────────────────────────────

var (
	pyDayNames   = map[string]bool{"mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true, "sun": true}
	pyMonthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec",
		"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
	pyTimezones = map[string]int{"UT": 0, "UTC": 0, "GMT": 0, "Z": 0, "AST": -400, "ADT": -300, "EST": -500, "EDT": -400,
		"CST": -600, "CDT": -500, "MST": -700, "MDT": -600, "PST": -800, "PDT": -700}
)

// parseEpoch is rss._parse_epoch: RFC 822 (email.utils.parsedate_to_datetime)
// then ISO 8601 (datetime.fromisoformat), naive values read as UTC, 0 on failure.
func parseEpoch(value string) float64 {
	value = providers.PyStrip(value)
	if value == "" {
		return 0
	}
	if micro, ok := parseRFC822(value); ok {
		return float64(micro) / 1e6
	}
	if micro, ok := parseISO(strings.ReplaceAll(value, "Z", "+00:00")); ok {
		return float64(micro) / 1e6
	}
	return 0
}

func atoiPy(s string) (int, bool) {
	s = providers.PyStrip(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
	return n, err == nil
}

// parseRFC822 mirrors email.utils._parsedate_tz + parsedate_to_datetime.
func parseRFC822(s string) (int64, bool) {
	data := providers.PyFields(s)
	if len(data) == 0 {
		return 0, false
	}
	if strings.HasSuffix(data[0], ",") || pyDayNames[strings.ToLower(data[0])] {
		data = data[1:]
	} else if i := strings.LastIndex(data[0], ","); i >= 0 {
		data[0] = data[0][i+1:]
	}
	if len(data) == 3 {
		if stuff := strings.Split(data[0], "-"); len(stuff) == 3 {
			data = append(stuff, data[1:]...)
		}
	}
	if len(data) == 4 {
		st := data[3]
		i := strings.Index(st, "+")
		if i == -1 {
			i = strings.Index(st, "-")
		}
		if i > 0 {
			data = append(data[:3:3], st[:i], st[i:])
		} else {
			data = append(data, "")
		}
	}
	if len(data) < 5 {
		return 0, false
	}
	dd, mm, yy, tm, tz := data[0], data[1], data[2], data[3], data[4]
	if dd == "" || mm == "" || yy == "" {
		return 0, false
	}
	monthIdx := func(m string) int {
		for i, n := range pyMonthNames {
			if n == m {
				return i
			}
		}
		return -1
	}
	mm = strings.ToLower(mm)
	mi := monthIdx(mm)
	if mi < 0 {
		dd, mm = mm, strings.ToLower(dd)
		if mi = monthIdx(mm); mi < 0 {
			return 0, false
		}
	}
	month := mi%12 + 1
	dd = strings.TrimSuffix(dd, ",")
	if i := strings.Index(yy, ":"); i > 0 {
		yy, tm = tm, yy
	}
	yy = strings.TrimSuffix(yy, ",")
	if yy == "" {
		return 0, false
	}
	if yy[0] < '0' || yy[0] > '9' {
		yy, tz = tz, yy
	}
	if tm == "" || yy == "" {
		return 0, false
	}
	tm = strings.TrimSuffix(tm, ",")
	var thh, tmm, tss string
	switch parts := strings.Split(tm, ":"); {
	case len(parts) == 2:
		thh, tmm, tss = parts[0], parts[1], "0"
	case len(parts) == 3:
		thh, tmm, tss = parts[0], parts[1], parts[2]
	case len(parts) == 1 && strings.Contains(parts[0], "."):
		dp := strings.Split(parts[0], ".")
		switch len(dp) {
		case 2:
			thh, tmm, tss = dp[0], dp[1], "0"
		case 3:
			thh, tmm, tss = dp[0], dp[1], dp[2]
		default:
			return 0, false
		}
	default:
		return 0, false
	}
	year, ok1 := atoiPy(yy)
	day, ok2 := atoiPy(dd)
	hh, ok3 := atoiPy(thh)
	mi2, ok4 := atoiPy(tmm)
	ss, ok5 := atoiPy(tss)
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		return 0, false
	}
	if year < 100 {
		if year > 68 {
			year += 1900
		} else {
			year += 2000
		}
	}
	tz = strings.ToUpper(tz)
	off, haveOff := pyTimezones[tz]
	if !haveOff {
		if n, ok := atoiPy(tz); ok {
			off, haveOff = n, true
		}
		if haveOff && off == 0 && strings.HasPrefix(tz, "-") {
			haveOff = false
		}
	}
	secs := 0
	if haveOff && off != 0 {
		sign := 1
		if off < 0 {
			sign, off = -1, -off
		}
		secs = sign * ((off/100)*3600 + (off%100)*60)
	}
	return civilMicro(year, month, day, hh, mi2, ss, 0, secs)
}

// civilMicro validates a datetime like Python's constructor and returns the UTC
// epoch in microseconds after subtracting the offset (seconds east of UTC).
func civilMicro(y, mo, d, h, mi, s, micro, offSec int) (int64, bool) {
	if y < 1 || y > 9999 || mo < 1 || mo > 12 || d < 1 || h < 0 || h > 23 || mi < 0 || mi > 59 || s < 0 || s > 59 {
		return 0, false
	}
	t := time.Date(y, time.Month(mo), d, h, mi, s, 0, time.UTC)
	if t.Day() != d || int(t.Month()) != mo {
		return 0, false
	}
	return (t.Unix()-int64(offSec))*1_000_000 + int64(micro), true
}

var (
	isoDateExt   = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})`)
	isoDateBasic = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})`)
	isoWeekRE    = regexp.MustCompile(`^(\d{4})(-?)W(\d{2})(?:(-?)(\d))?`)
	isoTimeExt   = regexp.MustCompile(`^(\d{2})(?::(\d{2})(?::(\d{2})(?:[.,](\d+))?)?)?`)
	isoTimeBasic = regexp.MustCompile(`^(\d{2})(?:(\d{2})(?:(\d{2})(?:[.,](\d+))?)?)?`)
	isoTZExt     = regexp.MustCompile(`^([+-])(\d{2})(?::(\d{2})(?::(\d{2})(?:\.(\d{1,6})\d*)?)?)?$`)
	isoTZBasic   = regexp.MustCompile(`^([+-])(\d{2})(?:(\d{2})(?:(\d{2})(?:\.(\d{1,6})\d*)?)?)?$`)
)

// parseISO mirrors datetime.fromisoformat (Python 3.11+) for the shapes feeds
// use; naive values are UTC.
func parseISO(s string) (int64, bool) {
	var y, mo, d, rest = 0, 0, 0, ""
	switch {
	case isoWeekRE.MatchString(s):
		m := isoWeekRE.FindStringSubmatch(s)
		yr, _ := strconv.Atoi(m[1])
		wk, _ := strconv.Atoi(m[3])
		day := 1
		if m[5] != "" {
			day, _ = strconv.Atoi(m[5])
		}
		if (m[2] == "") != (m[4] == "" || m[5] == "") && m[5] != "" {
			return 0, false
		}
		if wk < 1 || wk > 53 || day < 1 || day > 7 {
			return 0, false
		}
		jan4 := time.Date(yr, 1, 4, 0, 0, 0, 0, time.UTC)
		monday := jan4.AddDate(0, 0, -((int(jan4.Weekday())+6)%7)+(wk-1)*7)
		dt := monday.AddDate(0, 0, day-1)
		if wk == 53 {
			if _, w := time.Date(yr, 12, 28, 0, 0, 0, 0, time.UTC).ISOWeek(); w < 53 {
				return 0, false
			}
		}
		y, mo, d = dt.Year(), int(dt.Month()), dt.Day()
		rest = s[len(m[0]):]
	case isoDateExt.MatchString(s):
		m := isoDateExt.FindStringSubmatch(s)
		y, _ = strconv.Atoi(m[1])
		mo, _ = strconv.Atoi(m[2])
		d, _ = strconv.Atoi(m[3])
		rest = s[len(m[0]):]
	case isoDateBasic.MatchString(s):
		m := isoDateBasic.FindStringSubmatch(s)
		y, _ = strconv.Atoi(m[1])
		mo, _ = strconv.Atoi(m[2])
		d, _ = strconv.Atoi(m[3])
		rest = s[len(m[0]):]
	default:
		return 0, false
	}
	if _, ok := civilMicro(y, mo, d, 0, 0, 0, 0, 0); !ok {
		return 0, false
	}
	if rest == "" {
		return civilMicro(y, mo, d, 0, 0, 0, 0, 0)
	}
	// one separator character, then time[tz]
	_, sz := firstRune(rest)
	rest = rest[sz:]
	tpart, tzpart := rest, ""
	if i := strings.IndexAny(rest, "+-"); i >= 0 {
		tpart, tzpart = rest[:i], rest[i:]
	}
	var m []string
	if strings.Contains(tpart, ":") {
		m = isoTimeExt.FindStringSubmatch(tpart)
	} else {
		m = isoTimeBasic.FindStringSubmatch(tpart)
	}
	if m == nil || len(m[0]) != len(tpart) {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, s2, micro := 0, 0, 0
	if m[2] != "" {
		mi, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		s2, _ = strconv.Atoi(m[3])
	}
	if m[4] != "" {
		f := m[4]
		if len(f) > 6 {
			f = f[:6]
		}
		micro, _ = strconv.Atoi(f + strings.Repeat("0", 6-len(f)))
	}
	off := 0
	if tzpart != "" {
		tm := isoTZExt.FindStringSubmatch(tzpart)
		if tm == nil {
			tm = isoTZBasic.FindStringSubmatch(tzpart)
		}
		if tm == nil {
			return 0, false
		}
		th, _ := strconv.Atoi(tm[2])
		tmin, tsec := 0, 0
		if tm[3] != "" {
			tmin, _ = strconv.Atoi(tm[3])
		}
		if tm[4] != "" {
			tsec, _ = strconv.Atoi(tm[4])
		}
		if th > 23 || tmin > 59 || tsec > 59 {
			return 0, false
		}
		off = th*3600 + tmin*60 + tsec
		if tm[1] == "-" {
			off = -off
		}
	}
	return civilMicro(y, mo, d, h, mi, s2, micro, off)
}

func firstRune(s string) (rune, int) {
	for i, r := range s {
		_ = i
		return r, len(string(r))
	}
	return 0, 0
}

// ── per-host politeness spacer ──────────────────────────────────────────────

// HostSpacer enforces a minimum interval between requests to one host (<= 1
// req/s/host, spec §7). Reservations are serialised, so concurrent pollers of
// one host queue instead of racing (Python's spacer is single-threaded).
type HostSpacer struct {
	Interval time.Duration
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error

	mu   sync.Mutex
	last map[string]time.Time
}

// NewHostSpacer returns a spacer with the real clock.
func NewHostSpacer(interval time.Duration) *HostSpacer { return &HostSpacer{Interval: interval} }

var defaultSpacer = NewHostSpacer(feedHostSpacing)

func (h *HostSpacer) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Wait blocks until host may be requested again and records the slot.
func (h *HostSpacer) Wait(ctx context.Context, host string) error {
	h.mu.Lock()
	if h.last == nil {
		h.last = map[string]time.Time{}
	}
	now := h.now()
	slot := now
	if prev, ok := h.last[host]; ok && prev.Add(h.Interval).After(now) {
		slot = prev.Add(h.Interval)
	}
	h.last[host] = slot
	if len(h.last) > 4096 { // bound memory: drop slots that have already elapsed
		for k, v := range h.last {
			if !v.After(now) {
				delete(h.last, k)
			}
		}
	}
	h.mu.Unlock()
	wait := slot.Sub(now)
	if wait <= 0 {
		return nil
	}
	if h.Sleep != nil {
		return h.Sleep(ctx, wait)
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ── FetchFeed ───────────────────────────────────────────────────────────────

// FetchFeed is rss.fetch_feed / _afetch: a spaced, egress-guarded conditional
// GET (no redirects, 1 MiB body cap, 10 s timeout). It never reports a fetch
// problem as a Go error: a blocked URL, transport failure or non-200 lands in
// FeedResult.Error exactly as Python's “.error“; only a cancelled context is
// returned as an error.
func FetchFeed(ctx context.Context, d SourceDeps, rawURL, etag, lastModified string) (FeedResult, error) {
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if err := d.spacer().Wait(ctx, host); err != nil {
		return FeedResult{}, err
	}
	if err := egress.CheckURL(rawURL, true); err != nil {
		return FeedResult{Error: "blocked_url: " + blockedReason(err)}, nil
	}
	if reason := feedHostRefusal(host); reason != "" {
		return FeedResult{Error: "blocked_url: " + reason}, nil
	}
	hdr := [][2]string{{"User-Agent", FeedUA}, {"Accept", feedAccept}}
	if etag != "" {
		hdr = append(hdr, [2]string{"If-None-Match", etag})
	}
	if lastModified != "" {
		hdr = append(hdr, [2]string{"If-Modified-Since", lastModified})
	}
	cctx, cancel := context.WithTimeout(ctx, feedTimeout)
	defer cancel()
	resp, err := providers.Do(cctx, d.client(), providers.Req{Method: http.MethodGet, URL: rawURL, Header: hdr, MaxBody: FeedMaxBody})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return FeedResult{}, cerr
		}
		if msg, ok := blockedText(err.Error()); ok {
			return FeedResult{Error: "blocked_url: " + msg}, nil
		}
		if strings.HasPrefix(err.Error(), "lookup ") { // DNS failure at the dial-time resolve
			return FeedResult{Error: "blocked_url: cannot resolve host: " + providers.Trunc200(err.Error())}, nil
		}
		return FeedResult{Error: providers.Trunc200(err.Error())}, nil
	}
	newETag, newLM := headerPtr(resp.Header, "ETag"), headerPtr(resp.Header, "Last-Modified")
	switch {
	case resp.Status == 304:
		return FeedResult{Status: 304, NotModified: true, ETag: firstPtr(etag, newETag), LastModified: firstPtr(lastModified, newLM)}, nil
	case resp.Status != 200:
		return FeedResult{Status: resp.Status, Error: fmt.Sprintf("http_%d", resp.Status)}, nil
	}
	maxEntries := d.feedMax()
	entries, _ := ParseFeed(decodeFeedBody(resp.Body, resp.Header.Get("Content-Type")), maxEntries)
	return FeedResult{Status: 200, Entries: entries, ETag: newETag, LastModified: newLM}, nil
}

func headerPtr(h http.Header, k string) *string {
	if v := h.Values(k); len(v) > 0 {
		s := strings.Join(v, ", ")
		return &s
	}
	return nil
}

// firstPtr is Python's `request_value or response_value` (empty counts as unset).
func firstPtr(req string, resp *string) *string {
	if req != "" {
		return &req
	}
	return resp
}

// blockedReason extracts the Python BlockedUrlError text from an egress error.
func blockedReason(err error) string {
	var be *egress.BlockedError
	if errors.As(err, &be) {
		return be.Reason
	}
	return err.Error()
}

// blockedText recovers the Reason from a dial-time rejection that reached us
// as text (the transport flattens it).
func blockedText(msg string) (string, bool) {
	const marker = "egress: blocked: "
	if i := strings.Index(msg, marker); i >= 0 {
		return msg[i+len(marker):], true
	}
	return "", false
}

var (
	numericHostRE = regexp.MustCompile(`^(?:0[xX][0-9a-fA-F]+|[0-9]+)(?:\.(?:0[xX][0-9a-fA-F]+|[0-9]+))*$`)
	dottedQuadRE  = regexp.MustCompile(`^(?:[0-9]{1,3}\.){3}[0-9]{1,3}$`)
)

// feedHostRefusal is the hardening the egress guard does not cover for a
// URL-literal host (documented deviation): *.localhost, CGNAT 100.64.0.0/10 and
// short-form / hex / decimal-integer IPv4 spellings that a libc resolver would
// turn into a loopback or private address. A name that RESOLVES into CGNAT is
// still governed by the shared egress guard.
func feedHostRefusal(host string) string {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Sprintf("host '%s' is blocked", h)
	}
	if numericHostRE.MatchString(h) && !dottedQuadRE.MatchString(h) {
		return fmt.Sprintf("host '%s' is blocked", h)
	}
	if ip, err := netip.ParseAddr(h); err == nil && ip.Unmap().Is4() {
		if b := ip.Unmap().As4(); b[0] == 100 && b[1]&0xC0 == 64 {
			return fmt.Sprintf("ip '%s' is private/blocked", ip.Unmap())
		}
	}
	return ""
}
