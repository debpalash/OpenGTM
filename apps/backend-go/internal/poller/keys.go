// Package poller is the Go port of the intent poller (services/poller). This
// file holds the frozen dedup-key construction: KEY_SCHEMA_VERSION = 1 is
// permanent (a change needs a backfill), and every function here is byte-equal
// to keys.py (vectors generated from the Python module).
package poller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"unicode"
)

// KeySchemaVersion is KEY_SCHEMA_VERSION; it is part of every signal event id.
const KeySchemaVersion = 1

// isPySpace is Python's str.isspace() (also what str.strip() and \s use).
func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// collapseSpace is re.sub(r"\s+", " ", s).
func collapseSpace(s string) string {
	return strings.Join(strings.FieldsFunc(s, isPySpace), " ")
}

var (
	nonAlnumRE = regexp.MustCompile(`[^a-z0-9 ]+`)
	suffixRE   = regexp.MustCompile(`(?i)\b(inc|incorporated|corp|corporation|llc|l\.l\.c|llp|ltd|limited|co|company|plc|lp|holdings|group|the)\b`)
	schemeRE   = regexp.MustCompile(`^https?://`)
	wwwRE      = regexp.MustCompile(`^www\.`)
)

// normName is sec_edgar._norm_name: lowercase, strip punctuation and legal
// suffixes, collapse whitespace.
func normName(name string) string {
	s := strings.ToLower(name)
	s = nonAlnumRE.ReplaceAllString(s, " ")
	s = suffixRE.ReplaceAllString(s, " ")
	return collapseSpace(s)
}

// parsePyInt is int(str) for the forms a CIK can take: optional surrounding
// whitespace, an optional sign, ASCII digits with single underscores between
// digits.
func parsePyInt(s string) (*big.Int, bool) {
	s = pyStrip(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return nil, false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '_' {
			return nil, false
		}
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(s, "_", ""), 10)
	if !ok {
		return nil, false
	}
	if neg {
		n.Neg(n)
	}
	return n, true
}

// padCIK is sec_edgar._pad_cik: f"{int(cik):010d}", ok=false when the value is
// empty or not an integer.
func padCIK(cik string) (string, bool) {
	if cik == "" {
		return "", false
	}
	n, ok := parsePyInt(cik)
	if !ok {
		return "", false
	}
	digits := new(big.Int).Abs(n).String()
	sign := ""
	width := 10
	if n.Sign() < 0 {
		sign = "-"
		width--
	}
	if len(digits) < width {
		digits = strings.Repeat("0", width-len(digits)) + digits
	}
	return sign + digits, true
}

// NormalizeTarget is normalize_target (spec 8.1): sec_cik:<cik> becomes the
// canonical 10-digit CIK, URLs lose scheme, www. and trailing slashes, company
// names go through the SEC name normalisation.
func NormalizeTarget(target string) string {
	t := pyStrip(target)
	if t == "" {
		return ""
	}
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "sec_cik:") {
		rest := low[strings.Index(low, ":")+1:]
		if cik, ok := padCIK(pyStrip(rest)); ok {
			return cik
		}
		return low
	}
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		u := schemeRE.ReplaceAllString(low, "")
		u = wwwRE.ReplaceAllString(u, "")
		return strings.TrimRight(u, "/")
	}
	return normName(t)
}

// StableCompanyID is stable_company_id: the CIK when known (padded when it
// parses, verbatim otherwise), else the normalised target. Never lead_id.
func StableCompanyID(resolvedCIK *string, target string) string {
	if resolvedCIK != nil && *resolvedCIK != "" {
		if p, ok := padCIK(*resolvedCIK); ok {
			return p
		}
		return *resolvedCIK
	}
	return NormalizeTarget(target)
}

var roleTierRules = []struct {
	tier    string
	needles []string
}{
	{"director", []string{"director", "board"}},
	{"c_level", []string{"chief", "president", "ceo", "cfo", "cto", "coo", "cmo", "cro", "ciso", "cco", "cpo"}},
	{"vp", []string{"vp", "vice president", "head of"}},
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// containsWord is re.search(r"\b" + needle + r"\b", s) with Python's Unicode
// word boundaries (the needles start and end with a letter).
func containsWord(s, needle string) bool {
	for from := 0; ; {
		i := strings.Index(s[from:], needle)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(needle)
		okStart, okEnd := true, true
		if start > 0 {
			r := []rune(s[:start])
			okStart = !isWordRune(r[len(r)-1])
		}
		if end < len(s) {
			for _, r := range s[end:] {
				okEnd = !isWordRune(r)
				break
			}
		}
		if okStart && okEnd {
			return true
		}
		from = start + 1
		for from < len(s) && s[from]&0xC0 == 0x80 {
			from++
		}
	}
}

// RoleTier is role_tier: director, c_level, vp or other, whole-word matches in
// rule order (director first so "director" never matches "cto").
func RoleTier(title string) string {
	t := strings.ToLower(title)
	for _, rule := range roleTierRules {
		for _, n := range rule.needles {
			if containsWord(t, n) {
				return rule.tier
			}
		}
	}
	return "other"
}

// IsExecutiveTitle is is_executive_title.
func IsExecutiveTitle(title string) bool {
	switch RoleTier(title) {
	case "c_level", "director", "vp":
		return true
	}
	return false
}

// NormalizeExecName is normalize_exec_name.
func NormalizeExecName(name string) string {
	return collapseSpace(strings.ToLower(pyStrip(name)))
}

// NormalizeTech is normalize_tech.
func NormalizeTech(tech string) string {
	return collapseSpace(strings.ToLower(pyStrip(tech)))
}

// SignalEventID is signal_event_id: the deterministic sha256 hex id of a signal
// event, sha256("v1|ws|kind|stable|signal_type|natural").
func SignalEventID(workspaceID, kind, stableCompany, signalType, naturalEventID string) string {
	raw := fmt.Sprintf("v%d|%s|%s|%s|%s|%s", KeySchemaVersion, workspaceID, kind, stableCompany, signalType, naturalEventID)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
