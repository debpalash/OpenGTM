package llm

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

var (
	fenceOpen  = regexp.MustCompile("^```(?:json)?\\s*")
	fenceClose = regexp.MustCompile("\\s*```$")
)

// ParseObject extracts one JSON object from an LLM reply: a bare object, an
// object inside a markdown fence, or the first parsable {...} embedded in prose.
//
// It replaces leadgen/llm.py _parse_json, which had three defects fixed here:
// the embedded-object regex only handled two nesting levels (and so returned
// the inner object of a deeper one), it did not understand braces inside
// strings, and non-object results (arrays, numbers, strings) were returned to
// callers that expect a dict. ok is false when no object is found.
func ParseObject(text string) (obj map[string]any, ok bool) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = fenceOpen.ReplaceAllString(text, "")
		text = fenceClose.ReplaceAllString(text, "")
	}
	if o, ok := unmarshalObject([]byte(text)); ok {
		return o, true
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		end := matchBrace(text, i)
		if end < 0 {
			continue
		}
		if o, ok := unmarshalObject([]byte(text[i : end+1])); ok {
			return o, true
		}
	}
	return nil, false
}

func unmarshalObject(b []byte) (map[string]any, bool) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return nil, false
	}
	var o map[string]any
	if err := json.Unmarshal(b, &o); err != nil || o == nil {
		return nil, false
	}
	return o, true
}

// matchBrace returns the index of the brace closing the one at s[start], or -1.
// Braces inside JSON strings are ignored.
func matchBrace(s string, start int) int {
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
