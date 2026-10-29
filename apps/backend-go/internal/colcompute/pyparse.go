package colcompute

import "fmt"

// A recursive-descent parser for CPython 3.13's expression grammar (ast.parse(mode="eval")).
// It builds the same tree shape and child order as ast.parse so that the BFS whitelist
// walk (formula.go) reports the same first offender, and reproduces CPython's SyntaxError
// text for the common failure modes.

const maxNodes = 10_000

type node struct {
	kind string
	val  any
	name string // Name.id, Attribute.attr
	op   string // BinOp, UnaryOp, BoolOp operator symbol
	x    *node
	y    *node
	z    *node
	list []*node
	ops  []string // Compare operators
	kids []*node  // ast.iter_child_nodes order
}

var (
	binOpKinds = map[string]string{"+": "Add", "-": "Sub", "*": "Mult", "/": "Div", "//": "FloorDiv", "%": "Mod", "@": "MatMult",
		"**": "Pow", "|": "BitOr", "^": "BitXor", "&": "BitAnd", "<<": "LShift", ">>": "RShift"}
	unaryKinds = map[string]string{"+": "UAdd", "-": "USub", "~": "Invert", "not": "Not"}
	cmpKinds   = map[string]string{"==": "Eq", "!=": "NotEq", "<": "Lt", "<=": "LtE", ">": "Gt", ">=": "GtE",
		"in": "In", "not in": "NotIn", "is": "Is", "is not": "IsNot"}
)

type (
	parseFail    struct{ idx int }
	parseSpecial struct {
		msg  string
		line int
	}
	parseLex      struct{}
	parseUnclosed struct{}
	parseBudget   struct{}
)

type parser struct {
	toks      []token
	lexErr    *synErr
	pos       int
	maxPos    int // furthest token index fetched (CPython: tokens[fill-1])
	nodes     int
	noInvalid int
}

// parseExpression parses src like ast.parse(src, mode="eval"). budget reports a formula that
// exceeds maxNodes (not a Python error).
func parseExpression(src string) (tree *node, serr *synErr, budget bool) {
	toks, lexErr := tokenize(src)
	p := &parser{toks: toks, lexErr: lexErr}
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		switch f := r.(type) {
		case parseBudget:
			budget = true
		case parseLex:
			serr = lexErr
		case parseUnclosed:
			serr = p.unclosed(p.toks[len(p.toks)-1])
		case parseSpecial:
			serr = p.resolveSpecial(f)
		case parseFail:
			serr = p.resolveFail(p.maxPos)
		default:
			panic(r)
		}
	}()
	return p.evalInput(), nil, false
}

// lastLine is the line of the furthest token the parser touched.
func (p *parser) lastLine() int {
	i := p.maxPos
	if i >= len(p.toks) {
		i = len(p.toks) - 1
	}
	if i < 0 {
		return 1
	}
	return p.toks[i].line
}

func (p *parser) unclosed(t token) *synErr {
	return &synErr{msg: fmt.Sprintf("'%c' was never closed", t.openCh), line: t.openLine}
}

// resolveSpecial mirrors _PyPegen_set_syntax_error for an error raised by an invalid_ rule:
// a tokenizer error further on wins, as does an unclosed bracket on an earlier line.
func (p *parser) resolveSpecial(f parseSpecial) *synErr {
	if p.lexErr != nil {
		return p.lexErr
	}
	if n := len(p.toks); n > 0 && p.toks[n-1].kind == tkEOFParen && p.lastLine() > p.toks[n-1].openLine {
		return p.unclosed(p.toks[n-1])
	}
	return &synErr{msg: f.msg, line: f.line}
}

func (p *parser) resolveFail(idx int) *synErr {
	if idx >= len(p.toks) {
		if p.lexErr != nil {
			return p.lexErr
		}
		idx = len(p.toks) - 1
	}
	t := p.toks[idx]
	switch t.kind {
	case tkIndent:
		return &synErr{msg: "unexpected indent", line: t.line}
	case tkDedent:
		return &synErr{msg: "unexpected unindent", line: t.line}
	}
	if p.lexErr != nil {
		return p.lexErr
	}
	if n := len(p.toks); n > 0 && p.toks[n-1].kind == tkEOFParen && t.line > p.toks[n-1].openLine {
		return p.unclosed(p.toks[n-1])
	}
	return &synErr{msg: "invalid syntax", line: t.line}
}

// ---- token access ----

func (p *parser) tok(i int) *token {
	if i >= len(p.toks) {
		if p.lexErr != nil {
			panic(parseLex{})
		}
		i = len(p.toks) - 1
	}
	if i > p.maxPos {
		p.maxPos = i
	}
	t := &p.toks[i]
	if t.kind == tkEOFParen { // the tokenizer fails the moment the parser asks for it
		panic(parseUnclosed{})
	}
	return t
}

// try runs f and, on a plain parse failure, rewinds and reports nil (PEG backtracking).
func (p *parser) try(f func() *node) (n *node) {
	save := p.pos
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(parseFail); ok {
				p.pos, n = save, nil
				return
			}
			panic(r)
		}
	}()
	return f()
}

func (p *parser) cur() *token { return p.tok(p.pos) }
func (p *parser) fail() {
	if p.pos > p.maxPos && p.pos < len(p.toks) {
		p.maxPos = p.pos
	}
	panic(parseFail{p.pos})
}
func (p *parser) isOp(s string) bool { t := p.cur(); return t.kind == tkOp && t.text == s }
func (p *parser) isKw(s string) bool { t := p.cur(); return t.kind == tkName && t.text == s }

func (p *parser) acceptOp(s string) bool {
	if p.isOp(s) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectOp(s string) {
	if !p.acceptOp(s) {
		p.fail()
	}
}

func (p *parser) expectKw(s string) {
	if !p.isKw(s) {
		p.fail()
	}
	p.pos++
}

func (p *parser) special(msg string, line int) {
	if p.noInvalid == 0 {
		panic(parseSpecial{msg: msg, line: line})
	}
}

// ---- node construction ----

func (p *parser) mk(kind string) *node {
	p.nodes++
	if p.nodes > maxNodes {
		panic(parseBudget{})
	}
	return &node{kind: kind}
}

func op(kind string) *node { return &node{kind: kind} }
func load() *node          { return &node{kind: "Load"} }
func present(ns ...*node) []*node {
	var out []*node
	for _, n := range ns {
		if n != nil {
			out = append(out, n)
		}
	}
	return out
}

func (p *parser) leaf(kind string) *node { return p.mk(kind) }

func (p *parser) binOp(l *node, sym string, r *node) *node {
	n := p.mk("BinOp")
	n.op, n.x, n.y = sym, l, r
	n.kids = []*node{l, op(binOpKinds[sym]), r}
	return n
}

func (p *parser) unary(sym string, operand *node) *node {
	n := p.mk("UnaryOp")
	n.op, n.x = sym, operand
	n.kids = []*node{op(unaryKinds[sym]), operand}
	return n
}

func (p *parser) tuple(elts []*node) *node {
	n := p.mk("Tuple")
	n.list = elts
	n.kids = append(append([]*node{}, elts...), load())
	return n
}

func (p *parser) listNode(elts []*node) *node {
	n := p.mk("List")
	n.list = elts
	n.kids = append(append([]*node{}, elts...), load())
	return n
}

// ---- grammar ----

func (p *parser) evalInput() *node {
	body := p.expressions()
	for p.cur().kind == tkNewline {
		p.pos++
	}
	if p.cur().kind != tkEOF {
		p.fail()
	}
	n := p.mk("Expression")
	n.x = body
	n.kids = []*node{body}
	return n
}

func (p *parser) startsExpression() bool {
	t := p.cur()
	switch t.kind {
	case tkName:
		if pyKeywords[t.text] {
			return t.text == "True" || t.text == "False" || t.text == "None" || t.text == "lambda" || t.text == "not" || t.text == "await"
		}
		return true
	case tkNumber, tkString:
		return true
	case tkOp:
		switch t.text {
		case "(", "[", "{", "-", "+", "~", "...":
			return true
		}
	}
	return false
}

func (p *parser) expressions() *node {
	first := p.expression()
	if !p.isOp(",") {
		return first
	}
	elts := []*node{first}
	for p.acceptOp(",") {
		if !p.startsExpression() {
			break
		}
		elts = append(elts, p.expression())
	}
	return p.tuple(elts)
}

func (p *parser) expression() *node {
	if p.isKw("lambda") {
		return p.lambda()
	}
	start := p.pos
	d := p.disjunction()
	if p.isKw("if") {
		ifPos := p.pos
		p.pos++
		test := p.try(p.disjunction)
		if test != nil {
			if p.isKw("else") {
				p.pos++
				if orelse := p.try(p.expression); orelse != nil {
					n := p.mk("IfExp")
					n.x, n.y, n.z = test, d, orelse
					n.kids = []*node{test, d, orelse}
					return n
				}
			} else if !p.isOp(":") {
				p.special("expected 'else' after 'if' expression", p.toks[start].line)
			}
		}
		p.pos = ifPos // the conditional expression failed: only the disjunction stands
		return d
	}
	p.forgotComma(start)
	return d
}

// forgotComma raises invalid_expression's "Perhaps you forgot a comma?" for two adjacent
// expressions inside brackets.
func (p *parser) forgotComma(start int) {
	first := p.toks[start]
	if first.kind == tkName && (first.text == "_" || first.text == "match" || first.text == "case" || first.text == "type") {
		return
	}
	if first.kind == tkName && start+1 < len(p.toks) && p.toks[start+1].kind == tkString {
		return
	}
	if first.kind == tkName && (first.text == "print" || first.text == "exec") && p.pos == start+1 {
		return
	}
	if p.noInvalid > 0 || !p.startsExpression() {
		return
	}
	save := p.pos
	p.noInvalid++
	n := p.try(p.expression)
	p.noInvalid--
	end := p.pos
	p.pos = save
	if n != nil && p.toks[end-1].level > 0 {
		panic(parseSpecial{msg: "invalid syntax. Perhaps you forgot a comma?", line: first.line})
	}
}

func (p *parser) lambda() *node {
	p.pos++
	for !p.isOp(":") {
		switch {
		case p.isOp("*") || p.isOp("**"):
			p.pos++
			if p.cur().kind == tkName && !pyKeywords[p.cur().text] {
				p.pos++
			}
		case p.isOp("/"):
			p.pos++
		case p.cur().kind == tkName && !pyKeywords[p.cur().text]:
			p.pos++
			if p.acceptOp("=") {
				p.expression()
			}
		default:
			p.fail()
		}
		if !p.acceptOp(",") {
			break
		}
	}
	p.expectOp(":")
	p.expression()
	return p.leaf("Lambda")
}

func (p *parser) disjunction() *node {
	first := p.conjunction()
	if !p.isKw("or") {
		return first
	}
	vals := []*node{first}
	for p.isKw("or") {
		save := p.pos
		p.pos++
		next := p.try(p.conjunction)
		if next == nil {
			p.pos = save
			break
		}
		vals = append(vals, next)
	}
	if len(vals) == 1 {
		return first
	}
	return p.boolOp("or", vals)
}

func (p *parser) conjunction() *node {
	first := p.inversion()
	if !p.isKw("and") {
		return first
	}
	vals := []*node{first}
	for p.isKw("and") {
		save := p.pos
		p.pos++
		next := p.try(p.inversion)
		if next == nil {
			p.pos = save
			break
		}
		vals = append(vals, next)
	}
	if len(vals) == 1 {
		return first
	}
	return p.boolOp("and", vals)
}

func (p *parser) boolOp(sym string, vals []*node) *node {
	n := p.mk("BoolOp")
	n.op, n.list = sym, vals
	kind := "And"
	if sym == "or" {
		kind = "Or"
	}
	n.kids = append([]*node{op(kind)}, vals...)
	return n
}

func (p *parser) inversion() *node {
	if p.isKw("not") {
		p.pos++
		return p.unary("not", p.inversion())
	}
	return p.comparison()
}

func (p *parser) compareOp() (string, bool) {
	t := p.cur()
	switch {
	case t.kind == tkOp:
		switch t.text {
		case "==", "!=", "<", "<=", ">", ">=":
			p.pos++
			return t.text, true
		}
	case t.kind == tkName && t.text == "in":
		p.pos++
		return "in", true
	case t.kind == tkName && t.text == "is":
		p.pos++
		if p.isKw("not") {
			p.pos++
			return "is not", true
		}
		return "is", true
	case t.kind == tkName && t.text == "not":
		if nx := p.tok(p.pos + 1); nx.kind == tkName && nx.text == "in" {
			p.pos += 2
			return "not in", true
		}
	}
	return "", false
}

func (p *parser) comparison() *node {
	left := p.bitOr()
	var ops []string
	var comps []*node
	for {
		save := p.pos
		sym, ok := p.compareOp()
		if !ok {
			break
		}
		right := p.try(p.bitOr)
		if right == nil {
			p.pos = save
			break
		}
		ops = append(ops, sym)
		comps = append(comps, right)
	}
	if ops == nil {
		return left
	}
	n := p.mk("Compare")
	n.x, n.ops, n.list = left, ops, comps
	n.kids = []*node{left}
	for _, o := range ops {
		n.kids = append(n.kids, op(cmpKinds[o]))
	}
	n.kids = append(n.kids, comps...)
	return n
}

// notAfterOperator are the operators invalid_arithmetic reports "'not' after an operator".
var notAfterOperator = map[string]bool{"+": true, "-": true, "*": true, "/": true, "//": true, "%": true, "@": true}

func (p *parser) binLevel(next func() *node, syms ...string) *node {
	left := next()
	for {
		t := p.cur()
		if t.kind != tkOp {
			return left
		}
		matched := false
		for _, s := range syms {
			if t.text == s {
				matched = true
				break
			}
		}
		if !matched {
			return left
		}
		save := p.pos
		p.pos++
		if notAfterOperator[t.text] && p.isKw("not") {
			p.special("'not' after an operator must be parenthesized", p.cur().line)
		}
		right := p.try(next)
		if right == nil {
			p.pos = save
			return left
		}
		left = p.binOp(left, t.text, right)
	}
}

func (p *parser) bitOr() *node  { return p.binLevel(p.bitXor, "|") }
func (p *parser) bitXor() *node { return p.binLevel(p.bitAnd, "^") }
func (p *parser) bitAnd() *node { return p.binLevel(p.shift, "&") }
func (p *parser) shift() *node  { return p.binLevel(p.sum, "<<", ">>") }
func (p *parser) sum() *node    { return p.binLevel(p.term, "+", "-") }
func (p *parser) term() *node   { return p.binLevel(p.factor, "*", "/", "//", "%", "@") }

func (p *parser) factor() *node {
	t := p.cur()
	if t.kind == tkOp && (t.text == "+" || t.text == "-" || t.text == "~") {
		p.pos++
		if p.isKw("not") {
			p.special("'not' after an operator must be parenthesized", p.cur().line)
		}
		return p.unary(t.text, p.factor())
	}
	return p.power()
}

func (p *parser) power() *node {
	base := p.awaitPrimary()
	if p.isOp("**") {
		save := p.pos
		p.pos++
		if right := p.try(p.factor); right != nil {
			return p.binOp(base, "**", right)
		}
		p.pos = save
	}
	return base
}

func (p *parser) awaitPrimary() *node {
	if p.isKw("await") {
		p.pos++
		p.primary()
		return p.leaf("Await")
	}
	return p.primary()
}

func (p *parser) primary() *node {
	n := p.atom()
	for {
		save := p.pos
		var next *node
		switch {
		case p.isOp("."):
			p.pos++
			next = p.try(func() *node {
				t := p.cur()
				if t.kind != tkName || pyKeywords[t.text] {
					p.fail()
				}
				p.pos++
				a := p.mk("Attribute")
				a.x, a.name = n, t.text
				a.kids = []*node{n, load()}
				return a
			})
		case p.isOp("("):
			p.pos++
			next = p.try(func() *node { return p.call(n) })
		case p.isOp("["):
			p.pos++
			next = p.try(func() *node {
				sl := p.slices()
				p.expectOp("]")
				s := p.mk("Subscript")
				s.x, s.y = n, sl
				s.kids = []*node{n, sl, load()}
				return s
			})
		default:
			return n
		}
		if next == nil {
			p.pos = save
			return n
		}
		n = next
	}
}

func (p *parser) call(fn *node) *node {
	var args, kws []*node
	seenKw, seenKwUnpack := false, false
	for !p.isOp(")") {
		start := p.pos
		switch {
		case p.isOp("*"):
			p.pos++
			if !p.startsExpression() {
				p.special("Invalid star expression", p.toks[start].line)
			}
			e := p.expression()
			s := p.mk("Starred")
			s.x = e
			args = append(args, s)
		case p.isOp("**"):
			p.pos++
			e := p.expression()
			k := p.mk("keyword")
			k.x = e
			k.kids = []*node{e}
			kws = append(kws, k)
			seenKwUnpack = true
		case p.cur().kind == tkName && !pyKeywords[p.cur().text] && p.tok(p.pos+1).kind == tkOp && p.tok(p.pos+1).text == "=":
			p.pos += 2
			if p.isOp(",") || p.isOp(")") {
				p.special("expected argument value expression", p.toks[start].line)
			}
			e := p.expression()
			k := p.mk("keyword")
			k.x = e
			k.kids = []*node{e}
			kws = append(kws, k)
			seenKw = true
		default:
			e := p.namedExprMode(false)
			if p.isKw("for") || p.isKw("async") {
				p.compClauses()
				e = p.leaf("GeneratorExp")
				if len(args)+len(kws) > 0 || p.isOp(",") && !(p.tok(p.pos+1).kind == tkOp && p.tok(p.pos+1).text == ")") {
					p.special("Generator expression must be parenthesized", p.toks[start].line)
				}
			}
			if seenKwUnpack {
				p.special("positional argument follows keyword argument unpacking", p.toks[start].line)
			} else if seenKw {
				p.special("positional argument follows keyword argument", p.toks[start].line)
			}
			args = append(args, e)
		}
		if !p.acceptOp(",") {
			break
		}
	}
	p.expectOp(")")
	n := p.mk("Call")
	n.x, n.list = fn, args
	n.kids = append(append([]*node{fn}, args...), kws...)
	return n
}

func (p *parser) slices() *node {
	first := p.sliceItem()
	if !p.isOp(",") {
		return first
	}
	elts := []*node{first}
	for p.acceptOp(",") {
		if p.isOp("]") {
			break
		}
		elts = append(elts, p.sliceItem())
	}
	return p.tuple(elts)
}

func (p *parser) sliceItem() *node {
	if p.isOp("*") {
		p.pos++
		e := p.bitOr()
		s := p.mk("Starred")
		s.x = e
		return s
	}
	var lower, upper, step *node
	if !p.isOp(":") {
		e := p.namedExpr()
		if !p.isOp(":") {
			return e
		}
		lower = e
	}
	p.expectOp(":")
	if !p.isOp(":") && !p.isOp(",") && !p.isOp("]") {
		upper = p.expression()
	}
	if p.acceptOp(":") {
		if !p.isOp(",") && !p.isOp("]") {
			step = p.expression()
		}
	}
	n := p.mk("Slice")
	n.x, n.y, n.z = lower, upper, step
	n.kids = present(lower, upper, step)
	return n
}

func (p *parser) namedExpr() *node { return p.namedExprMode(true) }

// exprName is _PyPegen_get_expr_name.
func exprName(n *node) string {
	switch n.kind {
	case "Attribute":
		return "attribute"
	case "Subscript":
		return "subscript"
	case "Starred":
		return "starred"
	case "Name":
		return "name"
	case "List":
		return "list"
	case "Tuple":
		return "tuple"
	case "Lambda":
		return "lambda"
	case "Call":
		return "function call"
	case "BoolOp", "BinOp", "UnaryOp":
		return "expression"
	case "GeneratorExp":
		return "generator expression"
	case "Yield":
		return "yield expression"
	case "Await":
		return "await expression"
	case "ListComp":
		return "list comprehension"
	case "SetComp":
		return "set comprehension"
	case "DictComp":
		return "dict comprehension"
	case "Dict":
		return "dict literal"
	case "Set":
		return "set display"
	case "JoinedStr":
		return "f-string expression"
	case "Compare":
		return "comparison"
	case "IfExp":
		return "conditional expression"
	case "NamedExpr":
		return "named expression"
	case "Constant":
		switch v := n.val.(type) {
		case nil:
			return "None"
		case bool:
			if v {
				return "True"
			}
			return "False"
		case Ellipsis:
			return "ellipsis"
		}
	}
	return "literal"
}

// namedExprMode is named_expression. assignMsgs selects invalid_named_expression's
// "Maybe you meant '=='" messages; call arguments use invalid_kwarg's instead.
func (p *parser) namedExprMode(assignMsgs bool) *node {
	t := p.cur()
	if t.kind == tkName && !pyKeywords[t.text] {
		nx := p.tok(p.pos + 1)
		if nx.kind == tkOp && nx.text == ":=" {
			p.pos += 2
			p.expression()
			return p.leaf("NamedExpr")
		}
		if assignMsgs && nx.kind == tkOp && nx.text == "=" && p.noInvalid == 0 {
			save := p.pos
			p.pos += 2
			p.noInvalid++
			rhs := p.try(p.bitOr)
			p.noInvalid--
			followedByAssign := rhs != nil && (p.isOp("=") || p.isOp(":="))
			p.pos = save
			if rhs != nil && !followedByAssign {
				p.special("invalid syntax. Maybe you meant '==' or ':=' instead of '='?", t.line)
			}
		}
	}
	e := p.expression()
	if p.noInvalid == 0 && p.isOp("=") {
		switch {
		case e.kind == "List" || e.kind == "Tuple" || e.kind == "GeneratorExp":
		case e.kind == "Constant" && !isNumLiteral(e):
		default:
			save := p.pos
			p.pos++
			p.noInvalid++
			rhs := p.try(p.bitOr)
			p.noInvalid--
			ok := rhs != nil && !(p.isOp("=") || p.isOp(":="))
			p.pos = save
			if assignMsgs && ok {
				p.special(fmt.Sprintf("cannot assign to %s here. Maybe you meant '==' instead of '='?", exprName(e)), t.line)
			}
			if !assignMsgs {
				p.special("expression cannot contain assignment, perhaps you meant \"==\"?", t.line)
			}
		}
	}
	return e
}

// isNumLiteral: a Constant that is not True/False/None (those are excluded from alt 3).
func isNumLiteral(n *node) bool {
	switch n.val.(type) {
	case nil, bool:
		return false
	}
	return true
}

func (p *parser) starNamedExpr() *node {
	if p.isOp("*") {
		start := p.pos
		p.pos++
		if !p.startsExpression() {
			p.special("Invalid star expression", p.toks[start].line)
		}
		e := p.bitOr()
		s := p.mk("Starred")
		s.x = e
		return s
	}
	return p.namedExpr()
}

func (p *parser) compClauses() {
	for {
		if p.isKw("async") {
			p.pos++
		}
		p.expectKw("for")
		for {
			if p.isOp("*") {
				p.pos++
			}
			p.bitOr()
			if !p.acceptOp(",") || p.isKw("in") {
				break
			}
		}
		p.expectKw("in")
		p.disjunction()
		for p.isKw("if") {
			p.pos++
			p.disjunction()
		}
		if !p.isKw("for") && !p.isKw("async") {
			return
		}
	}
}

func (p *parser) atom() *node {
	t := p.cur()
	switch t.kind {
	case tkName:
		switch t.text {
		case "True", "False", "None":
			p.pos++
			n := p.mk("Constant")
			switch t.text {
			case "True":
				n.val = true
			case "False":
				n.val = false
			}
			return n
		}
		if pyKeywords[t.text] {
			p.fail()
		}
		p.pos++
		n := p.mk("Name")
		n.name = t.text
		n.kids = []*node{load()}
		return n
	case tkNumber:
		p.pos++
		n := p.mk("Constant")
		n.val = t.num
		return n
	case tkString:
		return p.strings()
	case tkOp:
		switch t.text {
		case "...":
			p.pos++
			n := p.mk("Constant")
			n.val = Ellipsis{}
			return n
		case "(":
			return p.parenAtom()
		case "[":
			return p.listAtom()
		case "{":
			return p.braceAtom()
		}
	}
	p.fail()
	return nil
}

func (p *parser) strings() *node {
	first := p.pos
	var anyF, anyB, anyS bool
	var sb []byte
	var ss []rune
	for p.cur().kind == tkString {
		lit := p.cur().str
		p.pos++
		switch {
		case lit.isF:
			anyF = true
			anyS = true
		case lit.isBytes:
			anyB = true
			sb = append(sb, lit.val.([]byte)...)
		default:
			anyS = true
			ss = append(ss, []rune(lit.val.(string))...)
		}
	}
	if anyB && anyS {
		p.special("cannot mix bytes and nonbytes literals", p.toks[first].line)
		p.pos = first + 1
		p.fail()
	}
	if anyF {
		return p.leaf("JoinedStr")
	}
	n := p.mk("Constant")
	if anyB {
		if sb == nil {
			sb = []byte{}
		}
		n.val = sb
	} else {
		n.val = string(ss)
	}
	return n
}

func (p *parser) parenAtom() *node {
	open := p.pos
	p.pos++
	if p.acceptOp(")") {
		return p.tuple(nil)
	}
	if p.isKw("yield") {
		p.pos++
		if p.isKw("from") {
			p.pos++
			p.expression()
		} else if !p.isOp(")") {
			p.expressions()
		}
		p.expectOp(")")
		return p.leaf("Yield")
	}
	if p.isOp("**") {
		p.pos++
		p.bitOr()
		if p.isOp(")") {
			p.special("cannot use double starred expression here", p.toks[open].line)
		}
		p.fail()
	}
	first := p.starNamedExpr()
	if p.isKw("for") || p.isKw("async") {
		p.compClauses()
		p.expectOp(")")
		return p.leaf("GeneratorExp")
	}
	if p.isOp(",") {
		elts := []*node{first}
		for p.acceptOp(",") {
			if p.isOp(")") {
				break
			}
			elts = append(elts, p.starNamedExpr())
		}
		p.expectOp(")")
		return p.tuple(elts)
	}
	p.expectOp(")")
	if first.kind == "Starred" {
		p.special("cannot use starred expression here", p.toks[open].line)
		p.fail()
	}
	return first
}

func (p *parser) listAtom() *node {
	p.pos++
	if p.acceptOp("]") {
		return p.listNode(nil)
	}
	first := p.starNamedExpr()
	if p.isKw("for") || p.isKw("async") {
		p.compClauses()
		p.expectOp("]")
		return p.leaf("ListComp")
	}
	elts := []*node{first}
	for p.acceptOp(",") {
		if p.isOp("]") {
			break
		}
		elts = append(elts, p.starNamedExpr())
	}
	p.expectOp("]")
	return p.listNode(elts)
}

func (p *parser) braceAtom() *node {
	p.pos++
	if p.acceptOp("}") {
		return p.leaf("Dict")
	}
	isDict := false
	if p.isOp("**") {
		p.pos++
		p.bitOr()
		isDict = true
	} else {
		first := p.starNamedExpr()
		if p.acceptOp(":") {
			p.expression()
			isDict = true
			if p.isKw("for") || p.isKw("async") {
				p.compClauses()
				p.expectOp("}")
				return p.leaf("DictComp")
			}
		} else if p.isKw("for") || p.isKw("async") {
			_ = first
			p.compClauses()
			p.expectOp("}")
			return p.leaf("SetComp")
		}
	}
	for p.acceptOp(",") {
		if p.isOp("}") {
			break
		}
		switch {
		case p.isOp("**"):
			p.pos++
			p.bitOr()
		case isDict:
			p.expression()
			p.expectOp(":")
			p.expression()
		default:
			p.starNamedExpr()
		}
	}
	p.expectOp("}")
	if isDict {
		return p.leaf("Dict")
	}
	return p.leaf("Set")
}
