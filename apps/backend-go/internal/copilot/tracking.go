package copilot

import (
	"strings"
)

// AccountSignalTypes and AccountSignalCadences are services/signals/tracking.py's ACCOUNT_SIGNAL_TYPES and
// ACCOUNT_SIGNAL_CADENCES, in Python order.
var (
	AccountSignalTypes    = []string{"partnership_hiring", "leadership_change", "funding", "pricing_page_change"}
	AccountSignalCadences = []string{"hourly", "daily", "weekly"}
)

// TrackingRequest is the parsed account-tracking follow-up.
type TrackingRequest struct {
	OriginalQuery string
	Cadence       string
	SignalTypes   []string
}

// pyLowerText is str.lower() for the substring checks below (U+0130 becomes "i̇" as in Python).
func pyLowerText(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// hasTrackAccounts is _TRACK_ACCOUNTS_RE.search: \btrack\s+(these|those|the|my)\s+accounts?\b.
func hasTrackAccounts(rs []rune) bool {
	for p := 0; p < len(rs); p++ {
		if !boundaryAt(rs, p) {
			continue
		}
		e, ok := hasWord(rs, p, "track")
		if !ok {
			continue
		}
		s, ok := spaces(rs, e)
		if !ok {
			continue
		}
		for _, det := range []string{"these", "those", "the", "my"} {
			d, ok := hasWord(rs, s, det)
			if !ok {
				continue
			}
			s2, ok := spaces(rs, d)
			if !ok {
				continue
			}
			a, ok := hasWord(rs, s2, "account")
			if !ok {
				continue
			}
			if a < len(rs) && asciiFold(rs[a], 's') && boundaryAt(rs, a+1) {
				return true
			}
			if boundaryAt(rs, a) {
				return true
			}
		}
	}
	return false
}

func containsAny(s string, terms ...string) bool {
	for _, t := range terms {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// ExtractTrackingRequest is extract_signal_tracking_request: the cadence (default weekly) and the requested
// signal types in ACCOUNT_SIGNAL_TYPES order, or nil.
func ExtractTrackingRequest(text string) *TrackingRequest {
	raw := strings.TrimFunc(text, isPySpaceRune)
	if raw == "" || !hasTrackAccounts([]rune(raw)) {
		return nil
	}
	lowered := pyLowerText(raw)
	lrs := []rune(lowered)
	cadence := "weekly"
	for _, c := range AccountSignalCadences {
		found := false
		for i := 0; i < len(lrs) && !found; i++ {
			_, found = wordAt(lrs, i, c)
		}
		if found {
			cadence = c
			break
		}
	}
	var signals []string
	if strings.Contains(lowered, "partnership") && containsAny(lowered, "hiring", "jobs", "roles") {
		signals = append(signals, "partnership_hiring")
	}
	if containsAny(lowered, "leadership change", "leadership changes", "executive change", "executive changes") {
		signals = append(signals, "leadership_change")
	}
	if containsAny(lowered, "funding", "fundraise", "fundraising") {
		signals = append(signals, "funding")
	}
	if strings.Contains(lowered, "pricing") && containsAny(lowered, "change", "changes", "page") {
		signals = append(signals, "pricing_page_change")
	}
	if len(signals) == 0 {
		return nil
	}
	return &TrackingRequest{OriginalQuery: raw, Cadence: cadence, SignalTypes: signals}
}
