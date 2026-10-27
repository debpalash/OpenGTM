package collect

import (
	"encoding/json"
	"strings"
	"unicode"
)

// Scraped pages and what a model extracts from them are untrusted (a page can carry
// prompt-injection text). Review 3a L2: everything stored from them is typed and length-capped,
// and the score prompt quotes and delimits it. The prompt text otherwise stays the legacy one.

// Caps on AI-extracted lead fields (runes).
const (
	capCompany     = 200
	capDescription = 500
	capField       = 200
)

// isHiddenRune is a control character (Cc), a line/paragraph separator, a bidi embedding,
// override or isolate, a direction mark or a BOM: none belongs in a lead field or a prompt line.
func isHiddenRune(r rune) bool {
	return unicode.IsControl(r) || r == 0x2028 || r == 0x2029 || (r >= 0x202a && r <= 0x202e) ||
		(r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0xfeff
}

// cleanScraped replaces hidden characters with spaces and caps s at n runes (s[:n]).
func cleanScraped(s string, n int) string {
	if strings.IndexFunc(s, isHiddenRune) >= 0 {
		s = strings.Map(func(r rune) rune {
			if isHiddenRune(r) {
				return ' '
			}
			return r
		}, s)
	}
	return truncRunes(s, n)
}

// aiField is `value or default` for a model-extracted text field: only a string or a number is
// stored (an object or list reads as unset), cleaned and capped at n runes.
func aiField(v any, def string, n int) string {
	switch v.(type) {
	case string, float64, json.Number:
	default:
		return def
	}
	if !pyTruthy(v) {
		return def
	}
	if out := pyStrip(cleanScraped(pyStr(v), n)); out != "" {
		return out
	}
	return def
}

// aiLead is the web strategy's lead from ai_extract_company's object (nil when neither the model
// nor the search title gives a usable company name).
func aiLead(data map[string]any, name, href, body, city, query string) *Lead {
	aiName := ""
	if s, ok := data["company_name"].(string); ok {
		aiName = pyStrip(cleanScraped(s, capCompany))
	}
	if aiName == "" || runeLen(aiName) < 3 {
		aiName = name
	}
	if aiName == "" || runeLen(aiName) < 3 {
		return nil
	}
	return NewLead(Lead{Company: aiName, Website: href, Email: aiField(data["email"], "", capField),
		Phone: aiField(data["phone"], "", capField), City: aiField(data["city"], city, capField),
		State: aiField(data["state"], "", capField), Address: aiField(data["address"], "", capField),
		Description:    aiField(data["description"], truncRunes(body, 300), capDescription),
		Specialization: aiField(data["specialization"], query, capField), IndustryTags: aiField(data["industry_tags"], "", capField),
		CompanySize: aiField(data["employee_count"], "", capField), EmployeeCountExact: orInt(data["employee_count_exact"]),
		FoundedYear: aiField(data["founded_year"], "", capField), Technologies: aiField(data["technologies"], "", capField),
		ContactPerson: aiField(data["contact_person"], "", capField), Source: "web_search"})
}

// promptQuote is one untrusted value in the score prompt: hidden characters become spaces, double
// quotes become single quotes, capped at n runes, wrapped in double quotes.
func promptQuote(s string, n int) string {
	return `"` + strings.ReplaceAll(cleanScraped(s, n), `"`, "'") + `"`
}
