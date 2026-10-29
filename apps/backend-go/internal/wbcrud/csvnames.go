package wbcrud

import (
	_ "embed"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Header and key normalisation of services/workbook/csv_import.py. Python's
// str.lower / str.casefold / str.title use the full Unicode mappings (and its
// own Unicode version), so they come from pycase_tables.txt, generated from the
// real interpreter by legacy/python/scripts/gen_csv_vectors.py.

//go:embed pycase_tables.txt
var pyCaseTablesSrc string

type pyCaseTables struct {
	lower, title, fold map[rune]string
	cased              [][2]rune
}

var (
	pyCaseOnce sync.Once
	pyCase     pyCaseTables
)

func loadPyCase() *pyCaseTables {
	pyCaseOnce.Do(func() {
		t := pyCaseTables{lower: map[rune]string{}, title: map[rune]string{}, fold: map[rune]string{}}
		for _, line := range strings.Split(pyCaseTablesSrc, "\n") {
			f := strings.Fields(line)
			if len(f) < 3 || strings.HasPrefix(line, "#") {
				continue
			}
			cp, err := strconv.ParseInt(f[1], 16, 32)
			if err != nil {
				panic("pycase_tables.txt: " + line)
			}
			if f[0] == "C" {
				end, _ := strconv.ParseInt(f[2], 16, 32)
				t.cased = append(t.cased, [2]rune{rune(cp), rune(end)})
				continue
			}
			var sb strings.Builder
			for _, h := range f[2:] {
				r, _ := strconv.ParseInt(h, 16, 32)
				sb.WriteRune(rune(r))
			}
			switch f[0] {
			case "L":
				t.lower[rune(cp)] = sb.String()
			case "T":
				t.title[rune(cp)] = sb.String()
			case "F":
				t.fold[rune(cp)] = sb.String()
			}
		}
		pyCase = t
	})
	return &pyCase
}

var csvHeaderAliases = map[string]string{
	"company": "company", "company name": "company", "organization": "company", "organization name": "company",
	"account name": "company", "domain": "website", "company domain": "website", "website": "website",
	"website url": "website", "company website": "website", "email": "email", "email address": "email",
	"work email": "email", "business email": "email", "phone": "phone", "phone number": "phone",
	"mobile": "phone", "mobile phone": "phone", "contact": "contact_person", "contact name": "contact_person",
	"contact person": "contact_person", "full name": "contact_person", "person name": "contact_person",
	"title": "contact_title", "job title": "contact_title", "contact title": "contact_title",
	"linkedin": "linkedin_url", "linkedin url": "linkedin_url", "person linkedin": "linkedin_url",
	"person linkedin url": "linkedin_url", "twitter": "twitter_url", "twitter url": "twitter_url",
	"city": "city", "state": "state", "region": "state", "address": "address", "industry": "specialization",
	"specialization": "specialization", "company size": "company_size", "employee count": "company_size",
	"employees": "company_size", "description": "description", "company description": "description",
	"notes": "notes", "source": "source", "status": "status", "score": "score",
}

// pyLowerRune is str.lower() of one code point (context-free part).
func pyLowerRune(r rune) string {
	if s, ok := loadPyCase().lower[r]; ok {
		return s
	}
	return string(r)
}

func isASCIIAlnum(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') }

// lowerRuns lowercases s like Python and returns the maximal runs of
// [a-z0-9], i.e. the pieces left after re.sub(r"[^a-z0-9]+", sep, ...).
func lowerRuns(s string) []string {
	var runs []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			runs = append(runs, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		for _, lr := range pyLowerRune(r) {
			if isASCIIAlnum(lr) {
				cur.WriteRune(lr)
			} else {
				flush()
			}
		}
	}
	flush()
	return runs
}

// NormalizeHeader is normalize_header: lower-case, every run of characters
// outside [a-z0-9] becomes one space, trimmed.
func NormalizeHeader(value string) string { return strings.Join(lowerRuns(value), " ") }

// ColumnKey is column_key: a JSON-safe, formula-friendly key.
func ColumnKey(value string) string {
	key := strings.Join(lowerRuns(value), "_")
	if key == "" {
		key = "imported_column"
	}
	if key[0] >= '0' && key[0] <= '9' {
		key = "column_" + key
	}
	if len(key) > 100 {
		key = key[:100]
	}
	return key
}

// pyCasefold is str.casefold().
func pyCasefold(s string) string {
	fold := loadPyCase().fold
	var b strings.Builder
	for _, r := range s {
		if f, ok := fold[r]; ok {
			b.WriteString(f)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isCasedRune(r rune) bool {
	c := loadPyCase().cased
	lo, hi := 0, len(c)
	for lo < hi {
		m := (lo + hi) / 2
		switch {
		case r < c[m][0]:
			hi = m
		case r > c[m][1]:
			lo = m + 1
		default:
			return true
		}
	}
	return false
}

func isCaseIgnorable(r rune) bool {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) {
		return true
	}
	switch r {
	case '\'', '.', ':', 0xB7, 0x2018, 0x2019, 0x2024, 0x2027, 0xFE13, 0xFE52, 0xFE55, 0xFF07, 0xFF0E, 0xFF1A, 0x387, 0x55F, 0x5F4, 0x2D7:
		return true
	}
	return false
}

// finalSigma is the Final_Sigma condition at rs[i]: preceded by a cased letter
// and not followed by one, skipping case-ignorable characters.
func finalSigma(rs []rune, i int) bool {
	j := i - 1
	for j >= 0 && isCaseIgnorable(rs[j]) {
		j--
	}
	if j < 0 || !isCasedRune(rs[j]) {
		return false
	}
	k := i + 1
	for k < len(rs) && isCaseIgnorable(rs[k]) {
		k++
	}
	return k >= len(rs) || !isCasedRune(rs[k])
}

// pyTitle is str.title(): the first cased letter of every run of cased letters
// is title-cased, the rest lower-cased (any other character ends the run).
func pyTitle(s string) string {
	rs := []rune(s)
	var b strings.Builder
	prevCased := false
	for i, r := range rs {
		switch {
		case !prevCased:
			if t, ok := loadPyCase().title[r]; ok {
				b.WriteString(t)
			} else {
				b.WriteRune(r)
			}
		case r == 0x3A3 && finalSigma(rs, i):
			b.WriteRune(0x3C2)
		default:
			b.WriteString(pyLowerRune(r))
		}
		prevCased = isCasedRune(r)
	}
	return b.String()
}
