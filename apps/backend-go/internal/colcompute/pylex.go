package colcompute

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	unorm "golang.org/x/text/unicode/norm"
)

// This file is a port of the parts of CPython 3.13's tokenizer that decide what text is a
// valid Python expression and what SyntaxError message an invalid one produces. Nothing is
// evaluated here.

type tokKind int

const (
	tkName tokKind = iota
	tkNumber
	tkString
	tkOp
	tkNewline
	tkIndent
	tkDedent
	tkEOF      // ENDMARKER
	tkEOFParen // EOF while brackets are open (CPython: ERRORTOKEN with tok->done == E_EOF)
)

type strLit struct {
	val     any // string or []byte
	isBytes bool
	isF     bool
}

type token struct {
	kind  tokKind
	text  string
	line  int
	level int
	num   any
	str   strLit
	// tkEOFParen: the innermost unclosed bracket
	openCh   rune
	openLine int
}

// synErr is a SyntaxError (or IndentationError) with CPython's text.
type synErr struct {
	msg  string
	line int
}

func (e *synErr) String() string { return fmt.Sprintf("%s (<unknown>, line %d)", e.msg, e.line) }

var pyKeywords = map[string]bool{
	"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true, "async": true,
	"await": true, "break": true, "class": true, "continue": true, "def": true, "del": true, "elif": true,
	"else": true, "except": true, "finally": true, "for": true, "from": true, "global": true, "if": true,
	"import": true, "in": true, "is": true, "lambda": true, "nonlocal": true, "not": true, "or": true,
	"pass": true, "raise": true, "return": true, "try": true, "while": true, "with": true, "yield": true,
}

func isXIDStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.Is(unicode.Nl, r) || unicode.Is(unicode.Other_ID_Start, r)
}

func isXIDContinue(r rune) bool {
	return isXIDStart(r) || unicode.In(r, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc) || unicode.Is(unicode.Other_ID_Continue, r)
}

func isPotentialIdentStart(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 128
}

func isPotentialIdentChar(c rune) bool {
	return isPotentialIdentStart(c) || c >= '0' && c <= '9'
}

func isDigitRune(c rune) bool { return c >= '0' && c <= '9' }

func isHexRune(c rune) bool {
	return isDigitRune(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

type lexer struct {
	rs   []rune
	pos  int
	line int
	toks []token
	err  *synErr

	parens []struct {
		ch   rune
		line int
	}
}

const maxParenLevel = 200

func (lx *lexer) fail(line int, format string, a ...any) {
	if lx.err == nil {
		lx.err = &synErr{msg: fmt.Sprintf(format, a...), line: line}
	}
}

func (lx *lexer) emit(t token) {
	t.level = len(lx.parens)
	lx.toks = append(lx.toks, t)
}

// tokenize splits src into tokens. On a lexical error the tokens before it are returned
// together with the error (the parser raises it when it reaches that point).
func tokenize(src string) ([]token, *synErr) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	lx := &lexer{rs: []rune(src), line: 1}
	lx.run()
	return lx.toks, lx.err
}

func (lx *lexer) at(i int) rune {
	if i < len(lx.rs) {
		return lx.rs[i]
	}
	return -1
}

func (lx *lexer) run() {
	indents := []int{0}
	atLineStart := true
	for lx.err == nil {
		if atLineStart && len(lx.parens) == 0 {
			// measure indentation
			col := 0
			p := lx.pos
			for {
				c := lx.at(p)
				if c == ' ' {
					col++
				} else if c == '\t' {
					col = (col/8 + 1) * 8
				} else if c == '\f' {
					col = 0
				} else {
					break
				}
				p++
			}
			c := lx.at(p)
			if c == '#' || c == '\n' {
				// blank or comment-only line: no tokens
				for lx.at(p) != '\n' && lx.at(p) != -1 {
					p++
				}
				if lx.at(p) == '\n' {
					p++
					lx.line++
				}
				lx.pos = p
				if p >= len(lx.rs) {
					goto eof
				}
				continue
			}
			if c == -1 {
				lx.pos = p
				goto eof
			}
			lx.pos = p
			atLineStart = false
			switch {
			case col > indents[len(indents)-1]:
				indents = append(indents, col)
				lx.emit(token{kind: tkIndent, line: lx.line})
			case col < indents[len(indents)-1]:
				for col < indents[len(indents)-1] {
					indents = indents[:len(indents)-1]
					lx.emit(token{kind: tkDedent, line: lx.line})
				}
				if col != indents[len(indents)-1] {
					lx.fail(lx.line, "unindent does not match any outer indentation level")
					return
				}
			}
		}
		c := lx.at(lx.pos)
		switch {
		case c == -1:
			goto eof
		case c == ' ' || c == '\t' || c == '\f':
			lx.pos++
		case c == '#':
			for lx.at(lx.pos) != '\n' && lx.at(lx.pos) != -1 {
				lx.pos++
			}
		case c == '\n':
			lx.pos++
			if len(lx.parens) > 0 {
				lx.line++
				continue
			}
			lx.emit(token{kind: tkNewline, line: lx.line})
			lx.line++
			atLineStart = true
		case c == '\\':
			n := lx.at(lx.pos + 1)
			if n == '\n' {
				lx.pos += 2
				lx.line++
				if lx.pos >= len(lx.rs) {
					lx.fail(lx.line-1, "unexpected EOF while parsing")
					return
				}
				continue
			}
			if n == -1 {
				lx.fail(lx.line, "unexpected character after line continuation character")
				return
			}
			lx.fail(lx.line, "unexpected character after line continuation character")
			return
		case isDigitRune(c) || c == '.' && isDigitRune(lx.at(lx.pos+1)):
			lx.number()
		case isPotentialIdentStart(c):
			lx.word()
		case c == '"' || c == '\'':
			lx.stringLit(lx.pos, false, false, false)
		default:
			lx.operator()
		}
	}
	return
eof:
	if len(lx.parens) > 0 {
		top := lx.parens[len(lx.parens)-1]
		lx.toks = append(lx.toks, token{kind: tkEOFParen, line: lx.line, level: len(lx.parens), openCh: top.ch, openLine: top.line})
		return
	}
	if n := len(lx.toks); n == 0 || lx.toks[n-1].kind != tkNewline {
		lx.emit(token{kind: tkNewline, line: lx.line})
	}
	for len(indents) > 1 {
		indents = indents[:len(indents)-1]
		lx.emit(token{kind: tkDedent, line: lx.line})
	}
	lx.emit(token{kind: tkEOF, line: lx.line})
}

var ops3 = map[string]bool{"**=": true, "//=": true, ">>=": true, "<<=": true, "...": true}
var ops2 = map[string]bool{
	"**": true, "//": true, ">>": true, "<<": true, "<=": true, ">=": true, "==": true, "!=": true, "<>": true,
	"->": true, ":=": true, "+=": true, "-=": true, "*=": true, "/=": true, "%=": true, "&=": true, "|=": true,
	"^=": true, "@=": true,
}

func (lx *lexer) operator() {
	c := lx.rs[lx.pos]
	if c < 0x20 || c == 0x7f {
		lx.fail(lx.line, "invalid non-printable character U+%04X", c)
		return
	}
	for _, n := range []int{3, 2} {
		if lx.pos+n <= len(lx.rs) {
			s := string(lx.rs[lx.pos : lx.pos+n])
			if n == 3 && ops3[s] || n == 2 && ops2[s] {
				lx.pos += n
				lx.emit(token{kind: tkOp, text: s, line: lx.line})
				return
			}
		}
	}
	lx.pos++
	switch c {
	case '(', '[', '{':
		if len(lx.parens) >= maxParenLevel {
			lx.fail(lx.line, "too many nested parentheses")
			return
		}
		lx.parens = append(lx.parens, struct {
			ch   rune
			line int
		}{c, lx.line})
		lx.emit(token{kind: tkOp, text: string(c), line: lx.line})
		return
	case ')', ']', '}':
		if len(lx.parens) == 0 {
			lx.fail(lx.line, "unmatched '%c'", c)
			return
		}
		top := lx.parens[len(lx.parens)-1]
		want := map[rune]rune{'(': ')', '[': ']', '{': '}'}[top.ch]
		if want != c {
			if top.line != lx.line {
				lx.fail(lx.line, "closing parenthesis '%c' does not match opening parenthesis '%c' on line %d", c, top.ch, top.line)
			} else {
				lx.fail(lx.line, "closing parenthesis '%c' does not match opening parenthesis '%c'", c, top.ch)
			}
			return
		}
		lx.parens = lx.parens[:len(lx.parens)-1]
		lx.emit(token{kind: tkOp, text: string(c), line: lx.line})
		return
	}
	lx.emit(token{kind: tkOp, text: string(c), line: lx.line})
}

func (lx *lexer) word() {
	start := lx.pos
	p := start
	for isPotentialIdentChar(lx.at(p)) {
		p++
	}
	word := string(lx.rs[start:p])
	// string prefix?
	if len(word) <= 2 && (lx.at(p) == '"' || lx.at(p) == '\'') {
		var b, r, u, f bool
		ok := true
		for _, c := range strings.ToLower(word) {
			switch {
			case c == 'b' && !(b || u || f):
				b = true
			case c == 'u' && !(b || u || r || f):
				u = true
			case c == 'r' && !(r || u):
				r = true
			case c == 'f' && !(f || b || u):
				f = true
			default:
				ok = false
			}
		}
		if ok {
			lx.pos = p
			lx.stringLit(start, b, r, f)
			return
		}
	}
	// verify_identifier
	for i, r := range lx.rs[start:p] {
		valid := isXIDContinue(r)
		if i == 0 {
			valid = isXIDStart(r)
		}
		if !valid {
			if unicode.IsPrint(r) {
				lx.fail(lx.line, "invalid character '%c' (U+%04X)", r, r)
			} else {
				lx.fail(lx.line, "invalid non-printable character U+%04X", r)
			}
			return
		}
	}
	lx.pos = p
	if !isASCII(word) {
		word = unorm.NFKC.String(word)
	}
	lx.emit(token{kind: tkName, text: word, line: lx.line})
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// stringLit scans a string literal whose prefix (if any) starts at start; lx.pos is at the
// opening quote.
func (lx *lexer) stringLit(start int, isB, isR, isF bool) {
	startLine := lx.line
	q := lx.rs[lx.pos]
	triple := lx.at(lx.pos+1) == q && lx.at(lx.pos+2) == q
	bodyStart := lx.pos + 1
	p := lx.pos + 1
	if triple {
		bodyStart = lx.pos + 3
		p = bodyStart
	}
	line := lx.line
	for {
		c := lx.at(p)
		if c == -1 || (c == '\n' && !triple) {
			kind := "string"
			if isF {
				kind = "f-string"
			}
			if triple {
				lx.fail(startLine, "unterminated triple-quoted %s literal (detected at line %d)", kind, line)
			} else {
				lx.fail(startLine, "unterminated %s literal (detected at line %d)", kind, line)
			}
			return
		}
		if c == '\n' {
			line++
		}
		if c == '\\' {
			if lx.at(p+1) == '\n' {
				line++
			}
			if lx.at(p+1) == -1 {
				p++
				continue
			}
			p += 2
			continue
		}
		if c == q {
			if !triple {
				break
			}
			if lx.at(p+1) == q && lx.at(p+2) == q {
				break
			}
		}
		p++
	}
	body := lx.rs[bodyStart:p]
	end := p + 1
	if triple {
		end = p + 3
	}
	lit := strLit{isBytes: isB, isF: isF}
	if isF {
		if msg := fstringCheck(body, isR); msg != "" {
			lx.fail(startLine, "%s", msg)
			return
		}
		lit.val = string(body)
	} else {
		v, msg := decodeLiteral(body, isR, isB)
		if msg != "" {
			lx.fail(startLine, "%s", msg)
			return
		}
		lit.val = v
	}
	lx.pos = end
	lx.line = line
	lx.emit(token{kind: tkString, line: startLine, str: lit})
}

// fstringCheck reports the brace errors CPython raises for an f-string body. Replacement
// fields are not parsed (an f-string is rejected by the whitelist anyway).
func fstringCheck(body []rune, raw bool) string {
	depth := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '{' && depth == 0 && i+1 < len(body) && body[i+1] == '{':
			i++
		case c == '}' && depth == 0 && i+1 < len(body) && body[i+1] == '}':
			i++
		case c == '{':
			depth++
		case c == '}':
			if depth == 0 {
				return "f-string: single '}' is not allowed"
			}
			depth--
		case c == '\\' && !raw && i+1 < len(body):
			i++
		}
	}
	if depth > 0 {
		return "f-string: expecting '}'"
	}
	return ""
}

// decodeLiteral decodes the body of a str or bytes literal.
func decodeLiteral(body []rune, raw, isBytes bool) (any, string) {
	if isBytes {
		for _, r := range body {
			if r > 0x7f {
				return nil, "bytes can only contain ASCII literal characters"
			}
		}
	}
	if raw {
		if isBytes {
			return []byte(string(body)), ""
		}
		return string(body), ""
	}
	var sb strings.Builder
	var bb []byte
	emit := func(r rune) {
		if isBytes {
			bb = append(bb, byte(r))
		} else {
			sb.WriteRune(r)
		}
	}
	// position bookkeeping for unicodeescape errors: a non-ASCII char counts as 10
	// (CPython re-encodes it as \UXXXXXXXX before decoding)
	posOf := func(i int) int {
		n := 0
		for _, r := range body[:i] {
			if r > 0x7f {
				n += 10
			} else {
				n++
			}
		}
		return n
	}
	hex := func(i, n int) (rune, int) { // value, digits consumed
		v, k := rune(0), 0
		for k < n && i+k < len(body) && isHexRune(body[i+k]) {
			d, _ := strconv.ParseUint(string(body[i+k]), 16, 8)
			v = v*16 + rune(d)
			k++
		}
		return v, k
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			emit(c)
			continue
		}
		i++
		if i >= len(body) {
			emit('\\')
			break
		}
		e := body[i]
		switch e {
		case '\n':
		case '\\', '\'', '"':
			emit(e)
		case 'a':
			emit(7)
		case 'b':
			emit(8)
		case 'f':
			emit(12)
		case 'n':
			emit('\n')
		case 'r':
			emit('\r')
		case 't':
			emit('\t')
		case 'v':
			emit(11)
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v, k := rune(e-'0'), 1
			for k < 3 && i+k < len(body) && body[i+k] >= '0' && body[i+k] <= '7' {
				v = v*8 + rune(body[i+k]-'0')
				k++
			}
			i += k - 1
			if isBytes {
				v &= 0xff
			}
			emit(v)
		case 'x':
			v, k := hex(i+1, 2)
			if k < 2 {
				if isBytes {
					return nil, fmt.Sprintf("(value error) invalid \\x escape at position %d", i-1)
				}
				return nil, fmt.Sprintf("(unicode error) 'unicodeescape' codec can't decode bytes in position %d-%d: truncated \\xXX escape", posOf(i-1), posOf(i-1)+1+k)
			}
			i += 2
			emit(v)
		case 'u', 'U':
			if isBytes {
				emit('\\')
				emit(e)
				continue
			}
			n, name := 4, "\\uXXXX"
			if e == 'U' {
				n, name = 8, "\\UXXXXXXXX"
			}
			v, k := hex(i+1, n)
			if k < n {
				return nil, fmt.Sprintf("(unicode error) 'unicodeescape' codec can't decode bytes in position %d-%d: truncated %s escape", posOf(i-1), posOf(i-1)+k+1, name)
			}
			if v > 0x10ffff {
				return nil, fmt.Sprintf("(unicode error) 'unicodeescape' codec can't decode bytes in position %d-%d: illegal Unicode character", posOf(i-1), posOf(i-1)+k+1)
			}
			i += n
			if v >= 0xd800 && v <= 0xdfff {
				v = utf8.RuneError // lone surrogates cannot live in a Go string (documented deviation)
			}
			emit(v)
		case 'N':
			if isBytes {
				emit('\\')
				emit(e)
				continue
			}
			end := -1
			if i+1 < len(body) && body[i+1] == '{' {
				for j := i + 2; j < len(body); j++ {
					if body[j] == '}' {
						end = j
						break
					}
				}
			}
			if end < 0 {
				return nil, fmt.Sprintf("(unicode error) 'unicodeescape' codec can't decode bytes in position %d-%d: malformed \\N character escape", posOf(i-1), posOf(i-1)+1)
			}
			return nil, fmt.Sprintf("(unicode error) 'unicodeescape' codec can't decode bytes in position %d-%d: unknown Unicode character name", posOf(i-1), posOf(end))
		default:
			emit('\\')
			emit(e)
		}
	}
	if isBytes {
		if bb == nil {
			bb = []byte{}
		}
		return bb, ""
	}
	return sb.String(), ""
}

// ---- numbers (tokenizer.c tok_get, number branch) ----

func (lx *lexer) number() {
	startLine := lx.line
	start := lx.pos
	end, msg := scanNumber(lx.rs, start)
	if msg != "" {
		lx.fail(startLine, "%s", msg)
		return
	}
	text := string(lx.rs[start:end])
	val, vmsg := numberValue(text)
	if vmsg != "" {
		lx.fail(startLine, "%s", vmsg)
		return
	}
	lx.pos = end
	lx.emit(token{kind: tkNumber, text: text, line: startLine, num: val})
}

func scanNumber(rs []rune, start int) (int, string) {
	cur := start + 1
	nextc := func() rune {
		if cur < len(rs) {
			c := rs[cur]
			cur++
			return c
		}
		return -1
	}
	backup := func(c rune) {
		if c != -1 {
			cur--
		}
	}
	lookahead := func(test string) bool {
		saved := cur
		defer func() { cur = saved }()
		for _, t := range test {
			if nextc() != t {
				return false
			}
		}
		c := nextc()
		return !isPotentialIdentChar(c)
	}
	verifyEnd := func(c rune, kind string) (string, bool) {
		r := false
		switch c {
		case 'a':
			r = lookahead("nd")
		case 'e':
			r = lookahead("lse")
		case 'f':
			r = lookahead("or")
		case 'i':
			c2 := nextc()
			if c2 == 'f' || c2 == 'n' || c2 == 's' {
				r = true
			}
			backup(c2)
		case 'o':
			r = lookahead("r")
		case 'n':
			r = lookahead("ot")
		}
		if r {
			return "", true
		}
		if c >= 0 && c < 128 && isPotentialIdentChar(c) {
			return "invalid " + kind + " literal", false
		}
		return "", true
	}
	decimalTail := func() (rune, string) {
		var c rune
		for {
			for {
				c = nextc()
				if !isDigitRune(c) {
					break
				}
			}
			if c != '_' {
				return c, ""
			}
			c = nextc()
			if !isDigitRune(c) {
				backup(c)
				return 0, "invalid decimal literal"
			}
		}
	}
	finish := func(c rune) (int, string) {
		backup(c)
		return cur, ""
	}

	c := rs[start]
	stage := 0 // 0 integer part just read, 1 fraction, 2 exponent, 3 imaginary
	if c == '.' {
		c = nextc()
		stage = 1
	} else if c == '0' {
		c = nextc()
		switch c {
		case 'x', 'X':
			c = nextc()
			for {
				if c == '_' {
					c = nextc()
				}
				if !isHexRune(c) {
					backup(c)
					return 0, "invalid hexadecimal literal"
				}
				for {
					c = nextc()
					if !isHexRune(c) {
						break
					}
				}
				if c != '_' {
					break
				}
			}
			if m, ok := verifyEnd(c, "hexadecimal"); !ok {
				return 0, m
			}
			return finish(c)
		case 'o', 'O', 'b', 'B':
			isOct := c == 'o' || c == 'O'
			kind, in := "binary", func(r rune) bool { return r == '0' || r == '1' }
			if isOct {
				kind, in = "octal", func(r rune) bool { return r >= '0' && r < '8' }
			}
			c = nextc()
			for {
				if c == '_' {
					c = nextc()
				}
				if !in(c) {
					if isDigitRune(c) {
						return 0, fmt.Sprintf("invalid digit '%c' in %s literal", c, kind)
					}
					backup(c)
					return 0, "invalid " + kind + " literal"
				}
				for {
					c = nextc()
					if !in(c) {
						break
					}
				}
				if c != '_' {
					break
				}
			}
			if isDigitRune(c) {
				return 0, fmt.Sprintf("invalid digit '%c' in %s literal", c, kind)
			}
			if m, ok := verifyEnd(c, kind); !ok {
				return 0, m
			}
			return finish(c)
		default:
			nonzero := false
			for {
				if c == '_' {
					c = nextc()
					if !isDigitRune(c) {
						backup(c)
						return 0, "invalid decimal literal"
					}
				}
				if c != '0' {
					break
				}
				c = nextc()
			}
			if isDigitRune(c) {
				nonzero = true
				var m string
				c, m = decimalTail()
				if m != "" {
					return 0, m
				}
			}
			switch {
			case c == '.':
				c = nextc()
				stage = 1
			case c == 'e' || c == 'E':
				stage = 2
			case c == 'j' || c == 'J':
				stage = 3
			case nonzero:
				backup(c)
				return 0, "leading zeros in decimal integer literals are not permitted; use an 0o prefix for octal integers"
			default:
				if m, ok := verifyEnd(c, "decimal"); !ok {
					return 0, m
				}
				return finish(c)
			}
		}
	} else {
		var m string
		c, m = decimalTail()
		if m != "" {
			return 0, m
		}
		if c == '.' {
			c = nextc()
			stage = 1
		}
	}
	if stage == 1 && isDigitRune(c) {
		var m string
		c, m = decimalTail()
		if m != "" {
			return 0, m
		}
	}
	if stage <= 1 && (c == 'e' || c == 'E') {
		stage = 2
	}
	if stage == 2 {
		e := c
		c = nextc()
		if c == '+' || c == '-' {
			c = nextc()
			if !isDigitRune(c) {
				backup(c)
				return 0, "invalid decimal literal"
			}
		} else if !isDigitRune(c) {
			backup(c)
			if m, ok := verifyEnd(e, "decimal"); !ok {
				return 0, m
			}
			backup(e)
			return cur, ""
		}
		var m string
		c, m = decimalTail()
		if m != "" {
			return 0, m
		}
	}
	if stage == 3 || c == 'j' || c == 'J' {
		c = nextc()
		if m, ok := verifyEnd(c, "imaginary"); !ok {
			return 0, m
		}
	} else if m, ok := verifyEnd(c, "decimal"); !ok {
		return 0, m
	}
	return finish(c)
}

// numberValue converts a scanned numeric literal to its Python value.
func numberValue(text string) (any, string) {
	t := strings.ReplaceAll(text, "_", "")
	lower := strings.ToLower(t)
	switch {
	case strings.HasPrefix(lower, "0x"), strings.HasPrefix(lower, "0o"), strings.HasPrefix(lower, "0b"):
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[lower[1]]
		i, ok := new(big.Int).SetString(lower[2:], base)
		if !ok {
			return nil, "invalid numeric literal"
		}
		return i, ""
	case strings.HasSuffix(lower, "j"):
		f, err := strconv.ParseFloat(lower[:len(lower)-1], 64)
		if err != nil && !math.IsInf(f, 0) {
			return nil, "invalid imaginary literal"
		}
		return complex(0, f), ""
	case strings.ContainsAny(lower, ".e"):
		f, err := strconv.ParseFloat(lower, 64)
		if err != nil && !math.IsInf(f, 0) {
			return nil, "invalid decimal literal"
		}
		return f, ""
	}
	if len(lower) > maxStrDigits {
		return nil, fmt.Sprintf("Exceeds the limit (%d digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit - Consider hexadecimal for huge integer literals to avoid decimal conversion limits.", maxStrDigits, len(lower))
	}
	i, ok := new(big.Int).SetString(lower, 10)
	if !ok {
		return nil, "invalid decimal literal"
	}
	return i, ""
}
