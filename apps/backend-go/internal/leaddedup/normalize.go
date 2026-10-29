// Package leaddedup ports legacy/python/apps/api/services/dedup.py (blocking +
// Jaro-Winkler lead deduplication and the waterfall merge). Python's re, str
// and urllib semantics are Unicode and code-point based, so everything here
// works on runes and hand-written Unicode predicates (Go regexp \b is ASCII
// only), verified by Python-generated vectors (testdata/vectors.json).
package leaddedup

import (
	"net/netip"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// pyIsSpace mirrors CPython Py_UNICODE_ISSPACE (str.isspace, re \s, str.split).
func pyIsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20:
		return true
	case r == 0x85, r == 0xa0, r == 0x1680, r >= 0x2000 && r <= 0x200a,
		r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

func isCased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) ||
		unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)
}

func isCaseIgnorable(r rune) bool {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) {
		return true
	}
	switch r {
	case 0x27, 0x2e, 0x3a, 0xb7, 0x387, 0x55f, 0x5f4, 0x2018, 0x2019, 0x2024, 0x2027,
		0xfe13, 0xfe52, 0xfe55, 0xff07, 0xff0e, 0xff1a:
		return true
	}
	return false
}

// pyLower mirrors str.lower(): full case mapping, i.e. U+0130 becomes "i" plus
// U+0307 and a capital sigma becomes final sigma at the end of a word.
func pyLower(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i, r := range rs {
		switch r {
		case 0x130:
			b.WriteString("i̇")
		case 0x3a3:
			j := i - 1
			for j >= 0 && isCaseIgnorable(rs[j]) {
				j--
			}
			final := j >= 0 && isCased(rs[j])
			if final {
				k := i + 1
				for k < len(rs) && isCaseIgnorable(rs[k]) {
					k++
				}
				final = k == len(rs) || !isCased(rs[k])
			}
			if final {
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

// isWordRune is Python's re \w for str patterns: alnum or underscore.
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// foldRune is the re.IGNORECASE comparison form for the ASCII suffix words:
// simple lowercase plus the extra equivalences (dotless i, long s).
func foldRune(r rune) rune {
	r = unicode.ToLower(r)
	switch r {
	case 0x131:
		return 'i'
	case 0x17f:
		return 's'
	}
	return r
}

var companySuffixes = map[string]struct{}{}

func init() {
	for _, w := range strings.Split("pvt|private|ltd|limited|llp|llc|inc|incorporated|corp|corporation|"+
		"co|company|group|enterprises|solutions|technologies|tech|services|"+
		"consulting|consultants|india|global|international|intl", "|") {
		companySuffixes[w] = struct{}{}
	}
}

// stripSuffixWords removes every whole word equal (case-insensitively) to a
// company suffix: \b(...)\b with both ends anchored on word/non-word
// transitions is exactly a maximal word run equal to a suffix.
func stripSuffixWords(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		if !isWordRune(rs[i]) {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j := i
		for j < len(rs) && isWordRune(rs[j]) {
			j++
		}
		f := make([]rune, 0, j-i)
		for _, r := range rs[i:j] {
			f = append(f, foldRune(r))
		}
		if _, ok := companySuffixes[string(f)]; ok {
			i = j
			continue
		}
		b.WriteString(string(rs[i:j]))
		i = j
	}
	return b.String()
}

const noiseChars = ".-,&()'\"!@#$%^*+="

// collapseSpace implements re.sub(r"\s+", " ", s).strip().
func collapseSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if pyIsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// NormalizeCompany lowercases, replaces noise punctuation, drops legal-suffix
// words and collapses whitespace.
func NormalizeCompany(s string) string {
	if s == "" {
		return ""
	}
	s = pyStrip(pyLower(s))
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(noiseChars, r) {
			return ' '
		}
		return r
	}, s)
	return collapseSpace(stripSuffixWords(s))
}

var bracketedFuture = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)

func checkBracketedHost(h string) bool {
	if strings.HasPrefix(h, "v") {
		return bracketedFuture.MatchString(h)
	}
	a, err := netip.ParseAddr(h)
	return err == nil && !a.Is4()
}

// pyHostname mirrors urllib.parse.urlsplit(u).hostname for a string that starts
// with "http://" or "https://". ok=false where Python raises ValueError.
func pyHostname(u string) (host string, ok bool) {
	u = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(u)
	i := strings.Index(u, ":")
	rest := u[i+1:]
	netloc := ""
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		end := len(rest)
		if k := strings.IndexAny(rest, "/?#"); k >= 0 {
			end = k
		}
		netloc = rest[:end]
	}
	open, closeB := strings.Contains(netloc, "["), strings.Contains(netloc, "]")
	if open != closeB {
		return "", false
	}
	if open {
		_, after, _ := strings.Cut(netloc, "[")
		inner, _, _ := strings.Cut(after, "]")
		if !checkBracketedHost(inner) {
			return "", false
		}
	}
	// CPython _checknetloc
	for _, r := range netloc {
		if r >= 0x80 {
			stripped := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
			if nf := norm.NFKC.String(stripped); nf != stripped && strings.ContainsAny(nf, "/?#@:") {
				return "", false
			}
			break
		}
	}
	hostinfo := netloc
	if k := strings.LastIndex(netloc, "@"); k >= 0 {
		hostinfo = netloc[k+1:]
	}
	if _, br, found := strings.Cut(hostinfo, "["); found {
		host, _, _ = strings.Cut(br, "]")
	} else {
		host, _, _ = strings.Cut(hostinfo, ":")
	}
	if host == "" {
		return "", true
	}
	h, zone, hasZone := strings.Cut(host, "%")
	host = pyLower(h)
	if hasZone {
		host += "%" + zone
	}
	return host, true
}

// NormalizeDomain extracts the host from a URL or bare domain, without "www.".
func NormalizeDomain(url string) string {
	if url == "" {
		return ""
	}
	url = pyStrip(pyLower(url))
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	host, ok := pyHostname(url)
	if !ok {
		return strings.Trim(strings.ReplaceAll(pyLower(url), "www.", ""), "/")
	}
	return strings.TrimPrefix(host, "www.")
}

// NormalizePhone keeps the last 10 Unicode decimal digits.
func NormalizePhone(phone string) string {
	if phone == "" {
		return ""
	}
	var ds []rune
	for _, r := range phone {
		if unicode.IsDigit(r) {
			ds = append(ds, r)
		}
	}
	if len(ds) > 10 {
		ds = ds[len(ds)-10:]
	}
	return string(ds)
}

// NormalizeEmail lowercases and trims.
func NormalizeEmail(email string) string {
	if email == "" {
		return ""
	}
	return pyStrip(pyLower(email))
}
