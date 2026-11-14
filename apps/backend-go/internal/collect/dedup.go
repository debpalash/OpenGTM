package collect

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
)

// DedupThreshold is deduplicate_leads' default fuzzy threshold.
const DedupThreshold = 85

// Dedupe is pipeline.deduplicate_leads: leads are stably sorted by how much contact data they
// carry (website, email, phone, LinkedIn, contact person; most first) — in place, like Python's
// list.sort — and a lead is dropped when its lower-cased company name has a token_sort_ratio of at
// least threshold with a kept one, or (when COLLECT_DEDUP_DOMAIN_IDENTITY is on) when it shares
// an identity key (registrable website domain, or company email domain) with a kept one.
//
// The identity rules extend name-only matching, which misses the real-world alias cases: the same
// company harvested as "BSM" and "BSM Security" on bsm.nl, or "Northwave" and "Northwave Cyber
// Security" on northwave.nl. Domain equality is a strong same-company signal; shared consumer
// email providers and shared website platforms (substack, wordpress, …) are deliberately NOT
// identity — they belong to different companies.
//
// The gate mirrors DEFAULT_REGION's contract: unset keeps the legacy name-only behaviour that the
// recorded job vectors pin.
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
	seenIDs := map[string]bool{}
	identityOn := envFlag("COLLECT_DEDUP_DOMAIN_IDENTITY")
	for _, l := range leads {
		dup := false
		for _, name := range seen {
			if TokenSortRatio(pyLower(l.Company), pyLower(name)) >= threshold {
				dup = true
				break
			}
		}
		var ids []string
		if identityOn {
			ids = leadIdentityKeys(l)
		}
		if !dup {
			for _, k := range ids {
				if seenIDs[k] {
					dup = true
					break
				}
			}
		}
		if !dup {
			unique = append(unique, l)
			seen = append(seen, l.Company)
			for _, k := range ids {
				seenIDs[k] = true
			}
		}
	}
	return unique
}

// envFlag reads an on/off env var through the geo.go getenv seam (tests override getenv).
func envFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(name))) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	}
	return false
}

// leadIdentityKeys are the strong same-company keys of a lead: "d:<registrable>" for its website
// host and "m:<mail domain>" for a non-consumer company email. Empty host, platform hosts and
// free-provider mail domains yield no key (they are shared between companies).
func leadIdentityKeys(l *Lead) []string {
	var keys []string
	if d := registrableDomain(DomainFromURL(l.Website)); d != "" {
		keys = append(keys, "d:"+d)
	}
	if l.Email != "" && strings.Contains(l.Email, "@") {
		if at := strings.LastIndex(l.Email, "@"); at > 0 {
			mdom := pyLower(pyStrip(l.Email[at+1:]))
			if mdom != "" && !emailverify.Classify(l.Email).IsFree {
				if d := registrableDomain(mdom); d != "" {
					keys = append(keys, "m:"+d)
				}
			}
		}
	}
	return keys
}

// sharedWebsiteHosts are website platforms whose host identifies a page, not a company: leads on
// the same platform host must never merge on host identity.
var sharedWebsiteHosts = setOf(
	"substack.com", "medium.com", "github.io", "wordpress.com", "wp.com", "blogger.com",
	"blogspot.com", "wixsite.com", "wix.com", "weebly.com", "squarespace.com", "carrd.co",
	"linkedin.com", "facebook.com", "twitter.com", "x.com", "instagram.com", "youtube.com",
	"crunchbase.com", "glassdoor.com", "indeed.com", "glassdoor.co.uk", "tracxn.com",
)

// secondLevelSuffixes are multi-part public suffixes (of the regions the pipeline targets) that
// must stay attached when deriving the registrable domain.
var secondLevelSuffixes = setOf(
	"co.uk", "org.uk", "ac.uk", "gov.uk", "me.uk", "net.uk", "ltd.uk", "plc.uk",
	"com.au", "net.au", "org.au", "co.in", "net.in", "org.in", "co.nz", "co.za",
	"com.br", "com.tr", "com.mx", "com.ar", "com.co", "co.id", "com.sg", "com.hk",
)

// registrableDomain reduces a host to its registrable domain: apex domain plus one label
// (three for known second-level suffixes). Blog/preview subdomains of shared platforms have no
// registrable identity here ("github.io" itself is a platform, excluded by sharedWebsiteHosts).
func registrableDomain(host string) string {
	host = pyLower(pyStrip(host))
	host = strings.TrimPrefix(host, "www.")
	if host == "" || !strings.Contains(host, ".") {
		return ""
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return ""
	}
	n := 2
	if len(labels) >= 3 && secondLevelSuffixes[strings.Join(labels[len(labels)-2:], ".")] {
		n = 3
	}
	reg := strings.Join(labels[len(labels)-n:], ".")
	if sharedWebsiteHosts[reg] {
		return ""
	}
	return reg
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
