package collect

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// DedupThreshold is deduplicate_leads' default fuzzy threshold.
const DedupThreshold = 85

// Dedupe is pipeline.deduplicate_leads: leads are stably sorted by how much contact data they
// carry (website, email, phone, LinkedIn, contact person; most first) — in place, like Python's
// list.sort — and a lead is dropped when its lower-cased company name has a token_sort_ratio of at
// least threshold with a kept one.
func Dedupe(leads []*Lead, threshold int) []*Lead {
	if len(leads) == 0 {
		return leads
	}
	score := func(l *Lead) int {
		n := 0
		for _, s := range []string{l.Website, l.Email, l.Phone, l.LinkedinURL, l.ContactPerson} {
			if s != "" {
				n++
			}
		}
		return n
	}
	sort.SliceStable(leads, func(i, j int) bool { return score(leads[i]) > score(leads[j]) })
	var unique []*Lead
	var seen []string
	for _, l := range leads {
		dup := false
		for _, name := range seen {
			if TokenSortRatio(pyLower(l.Company), pyLower(name)) >= threshold {
				dup = true
				break
			}
		}
		if !dup {
			unique = append(unique, l)
			seen = append(seen, l.Company)
		}
	}
	return unique
}

// TokenSortRatio is thefuzz.fuzz.token_sort_ratio(a, b) (force_ascii, full_process): characters
// U+0080..U+00FF are deleted, every non-alphanumeric character becomes a space, the text is
// lower-cased and stripped (rapidfuzz default_process), the whitespace tokens are sorted and
// joined, and the Indel similarity is returned as int(round(100 * ratio)).
func TokenSortRatio(a, b string) int {
	pa, pb := sortTokens(fullProcess(a)), sortTokens(fullProcess(b))
	return int(math.RoundToEven(indelRatio([]rune(pa), []rune(pb))))
}

func fullProcess(s string) string {
	var bl strings.Builder
	for _, r := range s {
		if r >= 128 && r <= 255 {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			bl.WriteRune(unicode.ToLower(r))
		} else {
			bl.WriteByte(' ')
		}
	}
	return pyStrip(bl.String())
}

func sortTokens(s string) string {
	toks := pySplit(s)
	sort.Strings(toks)
	return strings.Join(toks, " ")
}

// indelRatio is rapidfuzz fuzz.ratio: 100 * (1 - indel distance / (len a + len b)); two empty
// strings are 100.
func indelRatio(a, b []rune) float64 {
	total := len(a) + len(b)
	if total == 0 {
		return 100
	}
	// LCS by dynamic programming (names are short).
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	dist := total - 2*prev[len(b)]
	return 100 * (1 - float64(dist)/float64(total))
}
