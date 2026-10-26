// Package enrichcache is the workspace-scoped provider cache and email-verify
// cache. Keys and TTLs reproduce legacy/python enrichment/cache.py and
// email_verify_cache.py byte for byte (pinned by testdata/key_vectors.json);
// storage is Postgres under FORCE RLS (decision D5), so one tenant's entries
// never serve another.
package enrichcache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode"
)

// Lead carries the identity attributes the cache key is derived from.
type Lead struct {
	Website       string
	Company       string
	ContactPerson string
}

const defaultTTLDays = 30

var ttlDays = map[string]int{
	"email":        30,
	"phone":        60,
	"linkedin_url": 90,
	"company_size": 90,
	"description":  180,
	"founded_year": 365,
}

// TTLFor is the per-field freshness window (Python TTL_DAYS / DEFAULT_TTL_DAYS).
func TTLFor(field string) time.Duration {
	d, ok := ttlDays[field]
	if !ok {
		d = defaultTTLDays
	}
	return time.Duration(d) * 24 * time.Hour
}

// EmailTTL is 14 days for a definitive verdict (valid/invalid/catch_all) and
// one day for anything else (Python DEFINITIVE_TTL_DAYS / UNKNOWN_TTL_DAYS).
func EmailTTL(status string) time.Duration {
	switch status {
	case "valid", "invalid", "catch_all":
		return 14 * 24 * time.Hour
	}
	return 24 * time.Hour
}

var personFields = map[string]bool{
	"email": true, "phone": true, "linkedin_url": true, "contact_person": true, "contact_title": true,
}

// CanonicalKey is cache.canonical_key: ok=false when there is not enough
// identity to key on (the caller must not cache).
func CanonicalKey(field string, lead Lead) (string, bool) {
	domain := normDomain(lead.Website)
	company := normCompany(lead.Company)
	name := normName(lead.ContactPerson)

	var ident string
	if personFields[field] {
		org := domain
		if org == "" {
			org = company
		}
		if org == "" || name == "" {
			return "", false
		}
		ident = "person|" + name + "|" + org
	} else {
		org := domain
		if org == "" {
			org = company
		}
		if org == "" {
			return "", false
		}
		ident = "company|" + org
	}
	sum := sha256.Sum256([]byte(field + "|" + ident))
	return hex.EncodeToString(sum[:])[:24], true
}

// EmailHash is sha256 hex of the stripped, lower-cased address.
func EmailHash(email string) string {
	sum := sha256.Sum256([]byte(pyLower(pyStrip(email))))
	return hex.EncodeToString(sum[:])
}

// EmailKey is email_verify_cache.email_key: EmailHash + ":" + verifier.
func EmailKey(email, verifier string) string {
	return EmailHash(email) + ":" + verifier
}

// pyIsSpace matches Python str.isspace (adds the \x1c-\x1f separators).
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pyLower is str.lower: full case mapping, i.e. U+0130 becomes "i" + U+0307
// and a capital sigma at the end of a word becomes the final form.
func pyLower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case r == 0x130:
			b.WriteString("i̇")
		case r == 0x3a3:
			if i > 0 && unicode.IsLetter(rs[i-1]) && !(i+1 < len(rs) && unicode.IsLetter(rs[i+1])) {
				b.WriteRune(0x3c2)
			} else {
				b.WriteRune(0x3c3)
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func normName(name string) string {
	return strings.Join(strings.FieldsFunc(pyLower(pyStrip(name)), pyIsSpace), " ")
}

func normDomain(website string) string {
	if website == "" {
		return ""
	}
	d := pyLower(pyStrip(website))
	for _, p := range []string{"https://", "http://", "www."} {
		d = strings.TrimPrefix(d, p)
	}
	d = strings.SplitN(d, "/", 2)[0]
	d = strings.SplitN(d, "?", 2)[0]
	var parts []string
	for _, p := range strings.Split(d, ".") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], ".")
	}
	return d
}

var legalSuffixes = map[string]bool{
	"inc": true, "incorporated": true, "llc": true, "ltd": true, "limited": true, "corp": true,
	"corporation": true, "co": true, "company": true, "gmbh": true, "ag": true, "sarl": true,
	"sas": true, "sa": true, "bv": true, "plc": true, "pvt": true, "pte": true, "llp": true,
	"group": true, "holding": true, "holdings": true,
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

// stripLegalSuffixes reproduces the Python regex
// \b(inc|...|l\.l\.c|...|holdings?)\b\.? with a unicode-aware \b (RE2's is
// ASCII only): every match is a whole word-character run (or "l.l.c") plus an
// optional trailing dot.
func stripLegalSuffixes(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		if !isWord(rs[i]) || (i > 0 && isWord(rs[i-1])) {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j := i
		for j < len(rs) && isWord(rs[j]) {
			j++
		}
		end := -1
		if legalSuffixes[string(rs[i:j])] {
			end = j
		} else if string(rs[i:min(i+5, len(rs))]) == "l.l.c" && (i+5 == len(rs) || !isWord(rs[i+5])) {
			end = i + 5
		}
		if end < 0 {
			b.WriteString(string(rs[i:j]))
			i = j
			continue
		}
		if end < len(rs) && rs[end] == '.' {
			end++
		}
		i = end
	}
	return b.String()
}

func normCompany(name string) string {
	if name == "" {
		return ""
	}
	n := stripLegalSuffixes(pyLower(name))
	var b strings.Builder
	for _, r := range n {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
