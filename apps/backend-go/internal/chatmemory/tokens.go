// Package chatmemory is the builtin cross-conversation chat memory (port of
// _BuiltinMemory and its helpers in services/memory.py, PG path). chat_memories
// is a GLOBAL table (no workspace column, no RLS); tenant isolation relies on
// namespacing user_id exactly like Python: "{workspace}:{user|self-host}".
package chatmemory

import "strings"

var stopwords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "but": {}, "for": {}, "to": {}, "of": {}, "in": {}, "on": {}, "at": {},
	"is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "with": {}, "my": {}, "me": {}, "i": {}, "you": {}, "it": {},
	"this": {}, "that": {}, "show": {}, "find": {}, "get": {}, "what": {}, "how": {}, "do": {}, "does": {}, "can": {},
}

// Tokens is Python _tokens: lowercase, runs of [a-z0-9] longer than two
// characters, minus the stopwords.
func Tokens(text string) map[string]struct{} {
	// Python's str.lower() uses full case mapping, where U+0130 becomes "i" +
	// U+0307 (the combining dot then splits tokens); Go's ToLower yields "i".
	text = strings.ToLower(strings.ReplaceAll(text, "İ", "i̇"))
	out := map[string]struct{}{}
	start := -1
	flush := func(end int) {
		if start >= 0 && end-start > 2 {
			w := text[start:end]
			if _, stop := stopwords[w]; !stop {
				out[w] = struct{}{}
			}
		}
		start = -1
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(text))
	return out
}
