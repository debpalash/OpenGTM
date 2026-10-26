package dsnparse

import "strings"

// A line-for-line port of the parts of CPython 3.13 email.feedparser (compat32
// policy) that the bounce parser depends on. The corpus recorded from the real
// Python parser is the oracle, so quirks are preserved on purpose: folded
// header values keep their raw line endings, a bare CR ends a line, the newline
// before a boundary belongs to the boundary, and so on. Strings hold raw bytes
// (Python decodes input as ASCII with surrogateescape, so every byte >= 0x80 is
// a "surrogate" that makes a header value a non-str Header object).

const (
	// maxInput bounds the bytes handed to the parser (documented hardening, D6).
	maxInput = 1 << 20
	// maxDepth bounds MIME nesting; parts deeper than this are ignored (D6).
	maxDepth = 8
)

type header struct{ name, value string }

type part struct {
	headers     []header
	defaultType string
	payload     string // str payload (leaf); meaningful only when isStr
	isStr       bool
	isList      bool // payload is a list of sub-messages (Python is_multipart)
	children    []*part
	ctype       string // cached content type, "" until computed
}

// getRaw returns the first header with the given (lower-case) name.
func (p *part) getRaw(name string) (string, bool) {
	for i := range p.headers {
		if len(p.headers[i].name) == len(name) && asciiLower(p.headers[i].name) == name {
			return p.headers[i].value, true
		}
	}
	return "", false
}

func (p *part) contentType() string {
	if p.ctype != "" {
		return p.ctype
	}
	v, ok := p.getRaw("content-type")
	if !ok {
		p.ctype = p.defaultType
		return p.ctype
	}
	a, _, _ := strings.Cut(strValue(v), ";")
	c := asciiLower(pyStrip(a))
	if strings.Count(c, "/") != 1 {
		c = "text/plain"
	}
	p.ctype = c
	return c
}

func (p *part) mainType() string {
	m, _, _ := strings.Cut(p.contentType(), "/")
	return m
}

// param is Message.get_param(name) for the Content-Type header (unquoted).
func (p *part) param(name string) (string, bool) {
	v, ok := p.getRaw("content-type")
	if !ok {
		return "", false
	}
	for _, kv := range parseParams(strValue(v)) {
		if asciiLower(kv[0]) == name {
			return unquote(kv[1]), true
		}
	}
	return "", false
}

// boundary is Message.get_boundary(); ok is false when absent.
func (p *part) boundary() (string, bool) {
	b, ok := p.param("boundary")
	if !ok {
		return "", false
	}
	return pyRStrip(unquote(b)), true
}

// parseParams ports email.message._parseparam + _get_params_preserve for the
// non-RFC 2231 case: the first entry is the type itself.
func parseParams(s string) [][2]string {
	s = ";" + s
	var plist []string
	for strings.HasPrefix(s, ";") {
		s = s[1:]
		end := strings.IndexByte(s, ';')
		if end > 0 {
			// quote parity of s[:end]; scanned incrementally to stay linear.
			quotes, pairs, pos := 0, 0, 0
			for end > 0 {
				for ; pos < end; pos++ {
					if s[pos] == '"' {
						quotes++
						if pos > 0 && s[pos-1] == '\\' {
							pairs++
						}
					}
				}
				if (quotes-pairs)%2 == 0 {
					break
				}
				next := strings.IndexByte(s[end+1:], ';')
				if next < 0 {
					end = -1
				} else {
					end = end + 1 + next
				}
			}
		}
		if end < 0 {
			end = len(s)
		}
		f := s[:end]
		if i := strings.IndexByte(f, '='); i >= 0 {
			f = asciiLower(pyStrip(f[:i])) + "=" + pyStrip(f[i+1:])
		}
		plist = append(plist, pyStrip(f))
		s = s[end:]
	}
	out := make([][2]string, 0, len(plist))
	for i, p := range plist {
		name, val, found := strings.Cut(p, "=")
		if !found {
			name, val = pyStrip(p), ""
		} else {
			name, val = pyStrip(name), pyStrip(val)
		}
		if i > 0 {
			// decode_params: unquote, then re-quote and wrap; get_param
			// unquotes the wrapped value again.
			val = `"` + strings.ReplaceAll(strings.ReplaceAll(unquote(val), `\`, `\\`), `"`, `\"`) + `"`
		}
		out = append(out, [2]string{name, val})
	}
	return out
}

func unquote(s string) string {
	if len(s) > 1 {
		if s[0] == '"' && s[len(s)-1] == '"' {
			return strings.ReplaceAll(strings.ReplaceAll(s[1:len(s)-1], `\\`, `\`), `\"`, `"`)
		}
		if s[0] == '<' && s[len(s)-1] == '>' {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// ---------------------------------------------------------------- line source

type lineBuf struct {
	lines []string
	pos   int
	back  []string // unread stack, top is the next line
	eof   []func(string) bool
}

// splitLines splits on CRLF, CR or LF keeping the terminators.
func splitLines(s string) []string {
	var out []string
	for len(s) > 0 {
		i := strings.IndexAny(s, "\r\n")
		if i < 0 {
			out = append(out, s)
			break
		}
		j := i + 1
		if s[i] == '\r' && j < len(s) && s[j] == '\n' {
			j++
		}
		out = append(out, s[:j])
		s = s[j:]
	}
	return out
}

// readline returns the next line, or ok=false at (false) EOF.
func (b *lineBuf) readline() (string, bool) {
	var line string
	if n := len(b.back); n > 0 {
		line = b.back[n-1]
	} else if b.pos < len(b.lines) {
		line = b.lines[b.pos]
	} else {
		return "", false
	}
	for i := len(b.eof) - 1; i >= 0; i-- {
		if b.eof[i](line) {
			return "", false
		}
	}
	if n := len(b.back); n > 0 {
		b.back = b.back[:n-1]
	} else {
		b.pos++
	}
	return line, true
}

func (b *lineBuf) unread(line string) { b.back = append(b.back, line) }

func isBlank(line string) bool { return line != "" && (line[0] == '\r' || line[0] == '\n') }

// headerStart is headerRE.match: ^(From |[\041-\071\073-\176]*:|[\t ])
func headerStart(line string) bool {
	if strings.HasPrefix(line, "From ") {
		return true
	}
	if line == "" {
		return false
	}
	if line[0] == '\t' || line[0] == ' ' {
		return true
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == ':' {
			return true
		}
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return false
}

// ---------------------------------------------------------------- the parser

type parser struct {
	in    lineBuf
	stack []*part
	cur   *part
	last  *part
}

func (p *parser) newMessage() *part {
	m := &part{defaultType: "text/plain"}
	if p.cur != nil && p.cur.contentType() == "multipart/digest" {
		m.defaultType = "message/rfc822"
	}
	if n := len(p.stack); n > 0 {
		top := p.stack[n-1]
		top.isList = true
		top.children = append(top.children, m)
	}
	p.stack = append(p.stack, m)
	p.cur, p.last = m, m
	return m
}

func (p *parser) popMessage() {
	p.stack = p.stack[:len(p.stack)-1]
	if n := len(p.stack); n > 0 {
		p.cur = p.stack[n-1]
	} else {
		p.cur = nil
	}
}

func (p *parser) drain() []string {
	var lines []string
	for {
		line, ok := p.in.readline()
		if !ok {
			return lines
		}
		lines = append(lines, line)
	}
}

func (p *parser) setPayload(m *part, s string) { m.payload, m.isStr = s, true }

func (p *parser) readHeaders() []string {
	var hl []string
	for {
		line, ok := p.in.readline()
		if !ok {
			break
		}
		if !headerStart(line) {
			if !isBlank(line) {
				p.in.unread(line)
			}
			break
		}
		hl = append(hl, line)
	}
	return hl
}

func (p *parser) parseHeaders(m *part, lines []string) {
	lastheader := ""
	var lastvalue []string
	flush := func() {
		first := lastvalue[0]
		_, v, _ := strings.Cut(first, ":")
		v = strings.TrimLeft(v, " \t") + strings.Join(lastvalue[1:], "")
		m.headers = append(m.headers, header{name: first[:strings.IndexByte(first, ':')], value: strings.TrimRight(v, "\r\n")})
	}
	for lineno, line := range lines {
		if line[0] == ' ' || line[0] == '\t' {
			if lastheader == "" {
				continue
			}
			lastvalue = append(lastvalue, line)
			continue
		}
		if lastheader != "" {
			flush()
			lastheader, lastvalue = "", nil
		}
		if strings.HasPrefix(line, "From ") {
			if lineno == 0 {
				continue
			} else if lineno == len(lines)-1 {
				p.in.unread(line)
				return
			}
			continue
		}
		i := strings.IndexByte(line, ':')
		if i == 0 {
			continue
		}
		lastheader = line[:i]
		lastvalue = []string{line}
	}
	if lastheader != "" {
		flush()
	}
}

func boundaryMatcher(boundary string) func(string) (end bool, ok bool) {
	if strings.ContainsRune(boundary, '�') {
		// Python compares surrogate-escaped lines to a U+FFFD boundary: never equal.
		return func(string) (bool, bool) { return false, false }
	}
	sep := "--" + boundary
	return func(line string) (bool, bool) {
		if !strings.HasPrefix(line, sep) {
			return false, false
		}
		rest := line[len(sep):]
		end := false
		if strings.HasPrefix(rest, "--") {
			end = true
			rest = rest[2:]
		}
		rest = strings.TrimLeft(rest, " \t")
		switch rest {
		case "", "\r\n", "\r", "\n":
			return end, true
		}
		return false, false
	}
}

func stripEOL(s string) string {
	switch {
	case strings.HasSuffix(s, "\r\n"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "\n"), strings.HasSuffix(s, "\r"):
		return s[:len(s)-1]
	}
	return s
}

// parsegen parses one message from the line source; the caller pops it.
func (p *parser) parsegen(depth int) {
	m := p.newMessage()
	if depth > maxDepth {
		p.drain() // ignored (D6); payload stays empty
		return
	}
	p.parseHeaders(m, p.readHeaders())
	ct := m.contentType()
	if ct == "message/delivery-status" {
		for {
			p.in.eof = append(p.in.eof, isBlank)
			p.parsegen(depth) // blocks are siblings, not a deeper container
			p.popMessage()
			p.in.eof = p.in.eof[:len(p.in.eof)-1]
			p.in.readline()
			line, ok := p.in.readline()
			if !ok {
				break
			}
			p.in.unread(line)
		}
		return
	}
	if m.mainType() == "message" {
		p.parsegen(depth + 1)
		p.popMessage()
		return
	}
	if m.mainType() == "multipart" {
		p.multipart(m, depth)
		return
	}
	p.setPayload(m, strings.Join(p.drain(), ""))
}

func (p *parser) multipart(m *part, depth int) {
	boundary, ok := m.boundary()
	if !ok {
		p.setPayload(m, strings.Join(p.drain(), ""))
		return
	}
	match := boundaryMatcher(boundary)
	matchLine := func(line string) bool { _, ok := match(line); return ok }
	capturing := true
	var preamble []string
	closeSeen := false
	for {
		line, ok := p.in.readline()
		if !ok {
			break
		}
		end, isB := match(line)
		if !isB {
			preamble = append(preamble, line)
			continue
		}
		if end {
			closeSeen = true
			break
		}
		if capturing {
			capturing = false
			p.in.unread(line)
			continue
		}
		for {
			l, ok := p.in.readline()
			if !ok {
				break
			}
			if _, isB := match(l); !isB {
				p.in.unread(l)
				break
			}
		}
		p.in.eof = append(p.in.eof, matchLine)
		p.parsegen(depth + 1)
		if p.last.mainType() != "multipart" && p.last.isStr {
			p.last.payload = stripEOL(p.last.payload)
		}
		p.in.eof = p.in.eof[:len(p.in.eof)-1]
		p.popMessage()
		p.last = p.cur
	}
	if capturing {
		p.setPayload(m, strings.Join(preamble, ""))
		p.drain()
		return
	}
	if !closeSeen {
		return
	}
	p.drain() // epilogue
}

// parseMessage is email.message_from_bytes (bounded by the caller).
func parseMessage(raw []byte) *part {
	p := &parser{in: lineBuf{lines: splitLines(string(raw))}}
	p.parsegen(0)
	root := p.stack[0]
	return root
}

// rootHeaders parses only the header block of a message (Message.get use).
func rootHeaders(raw []byte) *part {
	p := &parser{in: lineBuf{lines: splitLines(string(raw))}}
	m := p.newMessage()
	p.parseHeaders(m, p.readHeaders())
	return m
}

// walk visits the part tree in pre-order (Message.walk).
func walk(root *part, fn func(*part)) {
	stack := []*part{root}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		fn(p)
		for i := len(p.children) - 1; i >= 0; i-- {
			stack = append(stack, p.children[i])
		}
	}
}
