package manifest

import "strings"

// domainOf ports _domain_of: strip, lowercase, drop scheme/www, cut at / and ?.
func domainOf(website string) string {
	if website == "" {
		return ""
	}
	d := lowerPy(pyStrip(website))
	for _, p := range []string{"https://", "http://", "www."} {
		d = strings.TrimPrefix(d, p)
	}
	d, _, _ = strings.Cut(d, "/")
	d, _, _ = strings.Cut(d, "?")
	return d
}

// lowerPy is strings.ToLower plus Python's full mapping of U+0130 (-> "i" +
// U+0307). Final-sigma context handling is not reproduced.
func lowerPy(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// InputContext ports _lead_input_ctx: the `input.*` values templates may
// reference. Missing lead keys read as "".
func InputContext(lead map[string]string) map[string]any {
	contact := lead["contact_person"]
	first, last := "", ""
	if contact != "" {
		parts := pyFields(contact)
		if len(parts) > 0 {
			first = parts[0]
		}
		if len(parts) > 1 {
			last = parts[len(parts)-1]
		}
	}
	website := lead["website"]
	return map[string]any{
		"company":        lead["company"],
		"company_name":   lead["company"],
		"website":        website,
		"domain":         domainOf(website),
		"email":          lead["email"],
		"linkedin_url":   lead["linkedin_url"],
		"city":           lead["city"],
		"contact_person": contact,
		"full_name":      contact,
		"first_name":     first,
		"last_name":      last,
		"phone":          lead["phone"],
	}
}
