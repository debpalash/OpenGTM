// Package provaccuracy is the Go port of the provider accuracy and freshness eval harness
// (leadgen/enrichment/eval): an offline, deterministic scorer of RECORDED provider outputs against a golden
// dataset. It yields the per-provider correctness prior that GET /api/workbooks/meta/provider-accuracy reports and
// (with ?persist=true) writes onto provider_stats for the waterfall planner.
//
// The golden set and the recorded outputs are embedded (data/); no network is ever touched. Vectors recorded from
// Python are in testdata/.
package provaccuracy

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// FreshnessDecayPerYear is the freshness weight a correct value loses per year of age.
	FreshnessDecayPerYear = 0.15
	// FreshnessWeight is the most freshness can contribute to a score; accuracy dominates.
	FreshnessWeight = 0.25
)

var wwwRE = regexp.MustCompile(`^(https?://)?(www\.)?`)

// pyStrip is str.strip() (Unicode whitespace plus the ASCII separators 0x1c-0x1f).
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// Normalize is _normalize: field-aware normalisation so trivially equal values compare equal (case, scheme, www,
// trailing slash, surrounding whitespace). A nil value is "".
func Normalize(field string, value any) string {
	if value == nil {
		return ""
	}
	s := strings.ToLower(pyStrip(pyStr(value)))
	if s == "" {
		return ""
	}
	switch {
	case field == "website" || field == "domain":
		s = strings.TrimRight(wwwRE.ReplaceAllString(s, ""), "/")
		if i := strings.IndexByte(s, '/'); i >= 0 {
			s = s[:i]
		}
	case strings.HasSuffix(field, "_url") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://"):
		s = strings.TrimRight(wwwRE.ReplaceAllString(s, ""), "/")
	case field == "founding_year" || field == "founded_year":
		if y, ok := firstYear(s); ok {
			s = y
		}
	}
	return s
}

// firstYear is re.search(r"\d{4}", s): the first run of four Unicode decimal digits.
func firstYear(s string) (string, bool) {
	rs := []rune(s)
	for i := 0; i+4 <= len(rs); i++ {
		ok := true
		for _, r := range rs[i : i+4] {
			if !unicode.Is(unicode.Nd, r) {
				ok = false
				break
			}
		}
		if ok {
			return string(rs[i : i+4]), true
		}
	}
	return "", false
}

// ValuesMatch is values_match: equal after normalisation, or for the free-text fields containment either way.
func ValuesMatch(field string, got, expected any) bool {
	a, b := Normalize(field, got), Normalize(field, expected)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if field == "industry_tags" || field == "description" {
		return strings.Contains(b, a) || strings.Contains(a, b)
	}
	return false
}

var asofRE = regexp.MustCompile(`^([0-9]{4})-([0-9]{1,2})-([0-9]{1,2})$`)

// Freshness is _freshness: 1.0 for a value dated today, decaying with age; undated or unparseable is 1.0.
func Freshness(asof any, today time.Time) float64 {
	s, ok := asof.(string)
	if !ok || s == "" {
		return 1.0
	}
	m := asofRE.FindStringSubmatch(s)
	if m == nil {
		return 1.0
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if y < 1 || mo < 1 || mo > 12 || d < 1 || t.Year() != y || int(t.Month()) != mo || t.Day() != d {
		return 1.0
	}
	t0 := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	days := math.Floor(t0.Sub(t).Hours() / 24)
	years := math.Max(0, days/365.25)
	return math.Max(0, 1.0-FreshnessDecayPerYear*years)
}

// Verdict is one scored field value.
type Verdict struct {
	CaseID    string
	Field     string
	Got       any
	Expected  any
	Correct   bool
	Freshness float64
}

// ProviderScore is the tally for one provider.
type ProviderScore struct {
	Provider     string
	Asserted     int     // field values returned (and truth known)
	Correct      int     // of those, matching golden
	GoldenSlots  int     // total golden field values it could have answered
	FreshnessSum float64 // sum of freshness over correct assertions
	Verdicts     []Verdict
}

// Accuracy is the correctness of what the provider asserted: the anti confident-wrong signal.
func (p ProviderScore) Accuracy() float64 {
	if p.Asserted == 0 {
		return 0
	}
	return float64(p.Correct) / float64(p.Asserted)
}

// Coverage is the share of golden slots answered.
func (p ProviderScore) Coverage() float64 {
	if p.GoldenSlots == 0 {
		return 0
	}
	return float64(p.Asserted) / float64(p.GoldenSlots)
}

// FreshnessAvg is the mean freshness of the correct assertions.
func (p ProviderScore) FreshnessAvg() float64 {
	if p.Correct == 0 {
		return 0
	}
	return p.FreshnessSum / float64(p.Correct)
}

// Score is the combined correctness prior in [0, 1]: accuracy dominates, freshness is a small multiplier.
func (p ProviderScore) Score() float64 {
	return p.Accuracy() * (1.0 - FreshnessWeight + FreshnessWeight*p.FreshnessAvg())
}

// Case is one golden case: an id and the expected field values (in file order).
type Case struct {
	ID       string
	Expected *OMap
}

// Payload is one provider's recorded output for one case.
type Payload struct {
	Fields *OMap
	AsOf   any
}

// Outputs is outputs[case_id][provider] in file order; a nil payload is a provider with no output.
type Outputs struct {
	Cases *OMap // case id -> *OMap (provider -> *Payload)
}

func nonEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	}
	return true
}

// ScoreOutputs is score_outputs. Provider order is first-seen order (it breaks ranking ties).
func ScoreOutputs(golden []Case, outputs Outputs, today time.Time) []*ProviderScore {
	byID := map[string]Case{}
	var order []string
	for _, c := range golden {
		if _, seen := byID[c.ID]; !seen {
			order = append(order, c.ID)
		}
		byID[c.ID] = c
	}
	slots := 0
	for _, id := range order {
		slots += byID[id].Expected.Len()
	}
	var scores []*ProviderScore
	index := map[string]*ProviderScore{}
	if outputs.Cases == nil {
		return nil
	}
	for _, caseID := range outputs.Cases.Keys() {
		c, ok := byID[caseID]
		if !ok {
			continue
		}
		byProv, _ := outputs.Cases.Get(caseID)
		pm, _ := byProv.(*OMap)
		if pm == nil {
			continue
		}
		for _, provider := range pm.Keys() {
			ps, ok := index[provider]
			if !ok {
				ps = &ProviderScore{Provider: provider, GoldenSlots: slots}
				index[provider] = ps
				scores = append(scores, ps)
			}
			pv, _ := pm.Get(provider)
			payload, _ := pv.(*Payload)
			var fields *OMap
			var asof any
			if payload != nil {
				fields, asof = payload.Fields, payload.AsOf
			}
			fresh := Freshness(asof, today)
			for _, fname := range c.Expected.Keys() {
				got, present := fields.Get(fname)
				if !present || !nonEmpty(got) {
					continue
				}
				exp, _ := c.Expected.Get(fname)
				ps.Asserted++
				ok := ValuesMatch(fname, got, exp)
				v := Verdict{CaseID: caseID, Field: fname, Got: got, Expected: exp, Correct: ok}
				if ok {
					ps.Correct++
					ps.FreshnessSum += fresh
					v.Freshness = fresh
				}
				ps.Verdicts = append(ps.Verdicts, v)
			}
		}
	}
	return scores
}

// Rank is rank_providers: highest score first, ties by accuracy then volume, then first-seen order (the sort is
// stable, like Python's sorted(..., reverse=True)).
func Rank(scores []*ProviderScore) []*ProviderScore {
	out := append([]*ProviderScore(nil), scores...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score() != b.Score() {
			return a.Score() > b.Score()
		}
		if a.Accuracy() != b.Accuracy() {
			return a.Accuracy() > b.Accuracy()
		}
		return a.Asserted > b.Asserted
	})
	return out
}

// Round3 is round(x, 3): correctly rounded from the exact binary value, ties to even.
func Round3(x float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 3, 64), 64)
	return f
}

// Float is a float64 that marshals as Python's json.dumps does (shortest repr, ".0" for integral values).
type Float float64

// MarshalJSON implements json.Marshaler.
func (f Float) MarshalJSON() ([]byte, error) { return []byte(pyFloatRepr(float64(f))), nil }

// Row is ProviderScore.to_api, fields in Python's key order.
type Row struct {
	Provider    string `json:"provider"`
	Accuracy    Float  `json:"accuracy"`
	Coverage    Float  `json:"coverage"`
	Freshness   Float  `json:"freshness"`
	Score       Float  `json:"score"`
	Asserted    int    `json:"asserted"`
	Correct     int    `json:"correct"`
	GoldenSlots int    `json:"golden_slots"`
}

// ToAPI is ProviderScore.to_api.
func (p ProviderScore) ToAPI() Row {
	return Row{
		Provider: p.Provider, Accuracy: Float(Round3(p.Accuracy())), Coverage: Float(Round3(p.Coverage())),
		Freshness: Float(Round3(p.FreshnessAvg())), Score: Float(Round3(p.Score())),
		Asserted: p.Asserted, Correct: p.Correct, GoldenSlots: p.GoldenSlots,
	}
}

// Rows maps a ranking to its API rows (never nil, so it marshals as []).
func Rows(ranking []*ProviderScore) []Row {
	rows := make([]Row, 0, len(ranking))
	for _, p := range ranking {
		rows = append(rows, p.ToAPI())
	}
	return rows
}
