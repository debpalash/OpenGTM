// Package safecond is the hardened, fail-closed condition evaluator that gates
// paid automation actions (apps/api/services/automations/safe_conditions.py).
//
// The evaluator never executes code: a condition is tokenized structurally
// first ({field} placeholders and quoted literals are atomic), cell values are
// bound as plain strings afterwards, and every parse or operand failure is a
// false result with a reason. Deviations from Python (D4): the condition is
// bounded to 4 KiB and 64 tokens and every resolved operand to 64 KiB; over
// the bound fails closed. Python is unbounded.
package safecond

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

const (
	maxConditionBytes = 4096
	maxTokens         = 64
	maxOperandLen     = 64 * 1024
)

// ConditionError is a parse or operand failure; its text is the structured
// reason (and is spliced into "invalid condition: <e>" at rule validation).
type ConditionError struct{ Msg string }

func (e *ConditionError) Error() string { return e.Msg }

func condErr(format string, a ...any) *ConditionError {
	return &ConditionError{Msg: fmt.Sprintf(format, a...)}
}

type tokKind uint8

const (
	tPlaceholder tokKind = iota
	tString
	tWord
	tOp
	tLParen
	tRParen
)

func (k tokKind) String() string {
	return [...]string{"PLACEHOLDER", "STRING", "WORD", "OP", "LPAREN", "RPAREN"}[k]
}

type token struct {
	kind  tokKind
	value string
}

var ops = [...]string{"==", "!=", ">=", "<=", ">", "<"}

func isWordStop(r rune) bool {
	if colgraph.PyIsSpace(r) {
		return true
	}
	switch r {
	case '(', ')', '{', '}', '"', '\'', '<', '>', '=', '!':
		return true
	}
	return false
}

// tokenize is _tokenize plus the size bounds. Placeholders and quoted strings
// are atomic, so their contents are never re-read as keywords.
func tokenize(expr string) ([]token, error) { return tokenizeLimit(expr, maxTokens) }

func tokenizeLimit(expr string, limit int) ([]token, error) {
	if len(expr) > maxConditionBytes {
		return nil, condErr("condition too long (more than %d bytes)", maxConditionBytes)
	}
	var toks []token
	add := func(k tokKind, v string) error {
		if len(toks) >= limit {
			return condErr("condition too complex (more than %d tokens)", limit)
		}
		toks = append(toks, token{k, v})
		return nil
	}
	i, n := 0, len(expr)
	for i < n {
		r, w := utf8.DecodeRuneInString(expr[i:])
		c := expr[i]
		switch {
		case colgraph.PyIsSpace(r):
			i += w
			continue
		case c == '{':
			j := strings.IndexByte(expr[i:], '}')
			if j < 0 {
				return nil, condErr("unterminated placeholder '{'")
			}
			if err := add(tPlaceholder, colgraph.PyStrip(expr[i+1:i+j])); err != nil {
				return nil, err
			}
			i += j + 1
			continue
		case c == '"' || c == '\'':
			j := strings.IndexByte(expr[i+1:], c)
			if j < 0 {
				return nil, condErr("unterminated string literal")
			}
			if err := add(tString, expr[i+1:i+1+j]); err != nil {
				return nil, err
			}
			i += j + 2
			continue
		case c == '(':
			if err := add(tLParen, "("); err != nil {
				return nil, err
			}
			i++
			continue
		case c == ')':
			if err := add(tRParen, ")"); err != nil {
				return nil, err
			}
			i++
			continue
		}
		matched := false
		for _, op := range ops {
			if strings.HasPrefix(expr[i:], op) {
				if err := add(tOp, op); err != nil {
					return nil, err
				}
				i += len(op)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		j := i
		for j < n {
			rr, ww := utf8.DecodeRuneInString(expr[j:])
			if isWordStop(rr) {
				break
			}
			j += ww
		}
		if j == i {
			return nil, condErr("unexpected character '%s'", string(r))
		}
		if err := add(tWord, expr[i:j]); err != nil {
			return nil, err
		}
		i = j
	}
	return toks, nil
}
