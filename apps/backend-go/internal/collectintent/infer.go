package collectintent

import (
	"strings"
	"unicode/utf8"
)

var relationNouns = []string{"companies", "businesses", "websites", "customers", "merchants", "teams"}
var relationNounsNoTeams = []string{"companies", "businesses", "websites", "customers", "merchants"}
var relationVerbsA = []string{"use", "using", "uses", "adopted", "powered by"}
var relationVerbsB = []string{"use", "using", "uses"}

// wordMatch reports whether word occurs at byte offset i of text with a
// Unicode \b on both sides (words start and end with word characters).
func wordMatch(text string, i int, word string) bool {
	if !strings.HasPrefix(text[i:], word) {
		return false
	}
	if i > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:i])
		if isPyWord(r) {
			return false
		}
	}
	end := i + len(word)
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if isPyWord(r) {
			return false
		}
	}
	return true
}

// relation emulates re.search(r"\b(?:A)\b.*\b(?:B)\b", text) for a text with
// no newlines (whitespace is collapsed before this is called): it matches iff
// some A-match ends at or before the start of some B-match.
func relation(text string, as, bs []string) bool {
	earliestEnd := -1
	lastStart := -1
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case 'c', 'b', 'w', 'm', 't', 'u', 'a', 'p':
		default:
			continue
		}
		if i > 0 && text[i-1] < 0x80 && isPyWord(rune(text[i-1])) {
			continue
		}
		for _, w := range as {
			if wordMatch(text, i, w) {
				if e := i + len(w); earliestEnd < 0 || e < earliestEnd {
					earliestEnd = e
				}
			}
		}
		for _, w := range bs {
			if wordMatch(text, i, w) {
				lastStart = i
			}
		}
		if earliestEnd >= 0 && lastStart >= earliestEnd {
			return true
		}
	}
	return earliestEnd >= 0 && lastStart >= earliestEnd
}

// Infer mirrors infer_collection_intent.
func Infer(query string) string {
	text := collapseWS(pyLower(pyStrip(query)))

	if containsAny(text, "monitor ", "watch ", "track changes", "alert me", "signals for") {
		return "signal_monitor"
	}
	if ExtractExplicitPeopleRequest(query) != nil || containsAny(text,
		"people at", "person at", "decision makers at", "decision-makers at",
		"employees at", "leadership at", "executives at", "contacts at") {
		return "people_at_company"
	}
	if relation(text, relationNouns, relationVerbsA) || relation(text, relationVerbsB, relationNounsNoTeams) {
		return "technology_users"
	}
	if containsAny(text, "research ", "company profile", "analyze company", "analyse company",
		"tell me about", "dossier", "account brief") {
		return "company_research"
	}
	return "market_search"
}

func containsAny(text string, terms ...string) bool {
	for _, t := range terms {
		if strings.Contains(text, t) {
			return true
		}
	}
	return false
}
