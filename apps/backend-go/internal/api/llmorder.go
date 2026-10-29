package api

import (
	"context"
	"sort"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// jsonSystemSuffix is the instruction llm.Client.ExtractJSON appends to the system prompt (unexported there).
// TestOrderedExtractMatchesClient pins both against drift by comparing the requests they send.
const jsonSystemSuffix = "IMPORTANT: Respond with valid JSON only. No markdown, no code fences, no explanation."

// orderedLLM is llm.Client.ExtractJSON that keeps the model's key order. The client parses the reply into an
// unordered map; the generator needs http_headers and http_body in the order the model wrote them, so this runs the
// same two attempts through Complete and re-reads the accepted object from the reply text.
type orderedLLM struct{ c *llm.Client }

// ExtractJSON mirrors llm.Client.ExtractJSON (same prompts, same retry) and returns the ordered object.
func (o orderedLLM) ExtractJSON(ctx context.Context, prompt, system string, maxTokens int) (*wbcrud.Obj, error) {
	jsonSystem := jsonSystemSuffix
	if system != "" {
		jsonSystem = system + "\n\n" + jsonSystemSuffix
	}
	lastErr := llm.ErrEmptyResponse
	for attempt := 0; attempt < 2; attempt++ {
		text, err := o.c.Complete(ctx, llm.CompleteRequest{Prompt: prompt, System: jsonSystem, MaxTokens: maxTokens})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if m, ok := llm.ParseObject(text); ok {
			return orderedObject(text, m), nil
		}
		lastErr = llm.ErrInvalidJSON
		if attempt == 0 {
			prompt += "\n\nYour previous response was not valid JSON. Return ONLY a JSON object, nothing else."
		}
	}
	return nil, lastErr
}

// orderedObject finds, in text, the object llm.ParseObject returned and decodes it in file order. When no
// candidate matches (it cannot happen for text ParseObject accepted) the map is used with sorted keys.
func orderedObject(text string, m map[string]any) *wbcrud.Obj {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		end := matchBrace(text, i)
		if end < 0 {
			continue
		}
		obj, err := wbcrud.ParseObjectCanon([]byte(text[i : end+1]))
		if err == nil && sameKeys(obj, m) {
			return obj
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := wbcrud.NewObj()
	for _, k := range keys {
		out.Set(k, m[k])
	}
	return out
}

func sameKeys(o *wbcrud.Obj, m map[string]any) bool {
	if o.Len() != len(m) {
		return false
	}
	for _, k := range o.Keys() {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

// matchBrace returns the index of the brace closing the object that opens at start (string-aware), or -1.
func matchBrace(s string, start int) int {
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
