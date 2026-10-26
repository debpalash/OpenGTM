package leaddedup

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// Rec is an ordered lead dict (Python dict insertion order). Values are
// string, int64/int, float64, bool, nil or JSON containers.
type Rec struct {
	Keys []string
	Vals map[string]any
}

// Get returns the value and whether the key exists.
func (r Rec) Get(k string) (any, bool) { v, ok := r.Vals[k]; return v, ok }

func (r Rec) str(k string) string {
	s, _ := r.Vals[k].(string)
	return s
}

// set assigns k, appending the key when new (dict assignment).
func (r *Rec) set(k string, v any) {
	if _, ok := r.Vals[k]; !ok {
		r.Keys = append(r.Keys, k)
	}
	r.Vals[k] = v
}

func (r Rec) clone() Rec {
	out := Rec{Keys: append([]string(nil), r.Keys...), Vals: make(map[string]any, len(r.Vals)+2)}
	for k, v := range r.Vals {
		out.Vals[k] = v
	}
	return out
}

// idOr is lead.get("id", def).
func (r Rec) idOr(def any) any {
	if v, ok := r.Vals["id"]; ok {
		return v
	}
	return def
}

// idKey normalises ids the way Python equality/hash does (1 == 1.0 == True).
func idKey(v any) any {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case bool:
		if x {
			return int64(1)
		}
		return int64(0)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<62 {
			return int64(x)
		}
		return x
	case nil, int64, string:
		return v
	}
	return fmt.Sprintf("%T:%v", v, v)
}

// FieldWeights are the Fellegi-Sunter style weights, in Python dict order.
var fieldNames = [5]string{"company", "domain", "phone", "email", "city"}
var fieldWeights = [5]float64{0.35, 0.30, 0.15, 0.10, 0.10}

// MatchResult is the comparison of two leads.
type MatchResult struct {
	LeadAID, LeadBID any
	Score            float64
	FieldScores      [5]float64 // company, domain, phone, email, city (rounded to 3)
	IsDuplicate      bool
}

type prepared struct {
	rec     Rec
	company string
	cRunes  []rune
	domain  string
	dRunes  []rune
	phone   string
	email   string
	city    string
	ciRunes []rune
}

func prepare(r Rec) *prepared {
	p := &prepared{rec: r}
	p.company = NormalizeCompany(r.str("company"))
	p.cRunes = []rune(p.company)
	p.domain = NormalizeDomain(r.str("website"))
	p.dRunes = []rune(p.domain)
	p.phone = NormalizePhone(r.str("phone"))
	p.email = NormalizeEmail(r.str("email"))
	p.city = pyStrip(pyLower(r.str("city")))
	p.ciRunes = []rune(p.city)
	return p
}

func emailDomain(e string) (string, bool) {
	if !strings.Contains(e, "@") {
		return "", false
	}
	return strings.Split(e, "@")[1], true
}

func compareP(a, b *prepared) MatchResult {
	var fs [5]float64
	if a.company != "" && b.company != "" {
		fs[0] = jaroWinklerRunes(a.cRunes, b.cRunes)
	}
	if a.domain != "" && b.domain != "" {
		if a.domain == b.domain {
			fs[1] = 1.0
		} else {
			fs[1] = jaroWinklerRunes(a.dRunes, b.dRunes)
		}
	}
	if a.phone != "" && b.phone != "" && utf8.RuneCountInString(a.phone) >= 7 && a.phone == b.phone {
		fs[2] = 1.0
	}
	if da, ok := emailDomain(a.email); ok {
		if db, ok := emailDomain(b.email); ok && da == db {
			fs[3] = 1.0
		}
	}
	if a.city != "" && b.city != "" {
		if a.city == b.city {
			fs[4] = 1.0
		} else {
			fs[4] = jaroWinklerRunes(a.ciRunes, b.ciRunes)
		}
	}
	var total float64
	for i := range fs {
		total = float64(total + float64(fs[i]*fieldWeights[i]))
	}
	if fs[1] == 1.0 {
		total = math.Max(total, 0.90)
	}
	if fs[2] == 1.0 {
		total = math.Max(total, 0.92)
	}
	res := MatchResult{
		LeadAID: a.rec.idOr(int64(0)), LeadBID: b.rec.idOr(int64(0)),
		Score: pyRound(total, 4), IsDuplicate: total >= 0.85,
	}
	for i := range fs {
		res.FieldScores[i] = pyRound(fs[i], 3)
	}
	return res
}

// Compare scores two leads (compare_leads).
func Compare(a, b Rec) MatchResult { return compareP(prepare(a), prepare(b)) }

// Cluster is one duplicate group; Key is the union-find root index as a string.
type Cluster struct {
	Key string
	IDs []any
}

// Pair is a scored duplicate pair.
type Pair struct {
	AID, BID    any
	Score       float64
	FieldScores [5]float64
}

// Suggestion is a merge suggestion for one cluster.
type Suggestion struct {
	MasterID           any
	MasterCompany      any
	DuplicateIDs       []any
	DuplicateCompanies []any
	Confidence         float64
}

// Stats are the summary counts (full form).
type Stats struct {
	TotalLeads, PairsCompared, DuplicatesFound, Clusters, UniqueLeads int
}

// Result is find_duplicates output. Short is the Python form for fewer than
// two leads (stats keys total/duplicates/unique). Scanned, when set, is
// appended as the trailing "scanned" key the route adds.
type Result struct {
	Short       bool
	Total       int
	Clusters    []Cluster
	Pairs       []Pair
	Stats       Stats
	Suggestions []Suggestion
	Scanned     *int
}

const blockCap = 100

type pairKey struct{ a, b int32 }

// FindDuplicates ports LeadDeduplicator.find_duplicates. Blocks are visited in
// first-seen order with a fixed per-lead key order (Python iterates a set, so
// discovery order is hash-seed dependent there; only member sets, pair sets and
// scores are seed independent).
func FindDuplicates(leads []Rec, threshold float64) Result {
	n := len(leads)
	if n < 2 {
		return Result{Short: true, Total: n}
	}
	preps := make([]*prepared, n)
	for i, l := range leads {
		preps[i] = prepare(l)
	}

	blockIdx := map[string]int{}
	var blocks [][]int32
	add := func(key string, i int) {
		bi, ok := blockIdx[key]
		if !ok {
			bi = len(blocks)
			blockIdx[key] = bi
			blocks = append(blocks, nil)
		}
		blocks[bi] = append(blocks[bi], int32(i))
	}
	for i, p := range preps {
		if rs := []rune(p.company); len(rs) >= 3 {
			add("name:"+string(rs[:3]), i)
		}
		if p.domain != "" {
			add("domain:"+p.domain, i)
		}
		if pr := []rune(p.phone); len(pr) >= 7 {
			add("phone:"+string(pr[len(pr)-7:]), i)
		}
		first := p.company
		if k := strings.IndexByte(first, ' '); k >= 0 {
			first = first[:k]
		}
		if p.city != "" && first != "" {
			add("city:"+p.city+":"+first, i)
		}
	}

	parent := make([]int32, n)
	rank := make([]int8, n)
	for i := range parent {
		parent[i] = int32(i)
	}
	find := func(x int32) int32 {
		r := x
		for parent[r] != r {
			r = parent[r]
		}
		for parent[x] != r {
			parent[x], x = r, parent[x]
		}
		return r
	}
	union := func(x, y int32) {
		rx, ry := find(x), find(y)
		if rx == ry {
			return
		}
		switch {
		case rank[rx] < rank[ry]:
			parent[rx] = ry
		case rank[rx] > rank[ry]:
			parent[ry] = rx
		default:
			parent[ry] = rx
			rank[rx]++
		}
	}

	compared := map[pairKey]struct{}{}
	var matches []MatchResult
	for _, idx := range blocks {
		if len(idx) < 2 || len(idx) > blockCap {
			continue
		}
		for i := 0; i < len(idx); i++ {
			for j := i + 1; j < len(idx); j++ {
				a, b := idx[i], idx[j]
				pk := pairKey{min(a, b), max(a, b)}
				if _, seen := compared[pk]; seen {
					continue
				}
				compared[pk] = struct{}{}
				res := compareP(preps[a], preps[b])
				res.LeadAID = leads[a].idOr(int64(a))
				res.LeadBID = leads[b].idOr(int64(b))
				if res.Score >= threshold {
					matches = append(matches, res)
					union(a, b)
				}
			}
		}
	}

	type group struct {
		root    int32
		members []int
	}
	var order []*group
	byRoot := map[int32]*group{}
	for i := 0; i < n; i++ {
		r := find(int32(i))
		g := byRoot[r]
		if g == nil {
			g = &group{root: r}
			byRoot[r] = g
			order = append(order, g)
		}
		g.members = append(g.members, i)
	}

	// leads by id key, in lead order
	byID := map[any][]int{}
	for i, l := range leads {
		k := idKey(l.Vals["id"])
		byID[k] = append(byID[k], i)
	}
	bestScore := map[any]float64{}
	for _, m := range matches {
		for _, id := range [2]any{m.LeadAID, m.LeadBID} {
			k := idKey(id)
			if v, ok := bestScore[k]; !ok || m.Score > v {
				bestScore[k] = m.Score
			}
		}
	}

	res := Result{}
	dupCount := 0
	for _, g := range order {
		if len(g.members) < 2 {
			continue
		}
		ids := make([]any, len(g.members))
		seen := map[any]struct{}{}
		var uniq []any
		for k, i := range g.members {
			ids[k] = leads[i].idOr(int64(i))
			ik := idKey(ids[k])
			if _, ok := seen[ik]; !ok {
				seen[ik] = struct{}{}
				uniq = append(uniq, ik)
			}
		}
		res.Clusters = append(res.Clusters, Cluster{Key: strconv.Itoa(int(g.root)), IDs: ids})
		dupCount += len(g.members) - 1

		var cl []int
		for _, ik := range uniq {
			cl = append(cl, byID[ik]...)
		}
		if len(cl) == 0 {
			continue
		}
		sort.Ints(cl)
		sort.SliceStable(cl, func(x, y int) bool { return completeness(leads[cl[x]]) > completeness(leads[cl[y]]) })
		master := leads[cl[0]]
		s := Suggestion{MasterID: master.Vals["id"], MasterCompany: getOr(master, "company", "")}
		for _, i := range cl[1:] {
			s.DuplicateIDs = append(s.DuplicateIDs, leads[i].Vals["id"])
			s.DuplicateCompanies = append(s.DuplicateCompanies, getOr(leads[i], "company", ""))
		}
		conf := 0.0
		first := true
		for _, ik := range uniq {
			if v, ok := bestScore[ik]; ok && (first || v > conf) {
				conf, first = v, false
			}
		}
		s.Confidence = conf
		res.Suggestions = append(res.Suggestions, s)
	}

	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	for _, m := range matches {
		res.Pairs = append(res.Pairs, Pair{AID: m.LeadAID, BID: m.LeadBID, Score: m.Score, FieldScores: m.FieldScores})
	}
	res.Stats = Stats{TotalLeads: n, PairsCompared: len(compared), DuplicatesFound: dupCount,
		Clusters: len(res.Clusters), UniqueLeads: n - dupCount}
	return res
}

func getOr(r Rec, k string, def any) any {
	if v, ok := r.Vals[k]; ok {
		return v
	}
	return def
}

var completenessFields = []string{"email", "phone", "website", "description", "linkedin_url",
	"contact_person", "company_size", "specialization"}

func completeness(r Rec) int {
	n := 0
	for _, f := range completenessFields {
		if v, ok := r.Vals[f]; ok && truthy(v) && !isEmptyMarker(v) {
			n++
		}
	}
	return n
}

// ---- ordered JSON writing (json.dumps, ensure_ascii=False, compact) ----

// MarshalJSON renders the Python result dict byte for byte.
func (r Result) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	w := &jw{b: &b}
	if r.Short {
		b.WriteString(`{"clusters":{},"pairs":[],"stats":{"total":`)
		fmt.Fprintf(&b, `%d,"duplicates":0,"unique":%d},"merge_suggestions":[]`, r.Total, r.Total)
	} else {
		b.WriteString(`{"clusters":{`)
		for i, c := range r.Clusters {
			if i > 0 {
				b.WriteByte(',')
			}
			w.str(c.Key)
			b.WriteByte(':')
			w.val(c.IDs)
		}
		b.WriteString(`},"pairs":[`)
		for i, p := range r.Pairs {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"lead_a_id":`)
			w.val(p.AID)
			b.WriteString(`,"lead_b_id":`)
			w.val(p.BID)
			b.WriteString(`,"score":`)
			w.float(p.Score)
			b.WriteString(`,"field_scores":`)
			w.fieldScores(p.FieldScores)
			b.WriteByte('}')
		}
		fmt.Fprintf(&b, `],"stats":{"total_leads":%d,"pairs_compared":%d,"duplicates_found":%d,"clusters":%d,"unique_leads":%d},"merge_suggestions":[`,
			r.Stats.TotalLeads, r.Stats.PairsCompared, r.Stats.DuplicatesFound, r.Stats.Clusters, r.Stats.UniqueLeads)
		for i, s := range r.Suggestions {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"master_id":`)
			w.val(s.MasterID)
			b.WriteString(`,"master_company":`)
			w.val(s.MasterCompany)
			b.WriteString(`,"duplicate_ids":`)
			w.val(s.DuplicateIDs)
			b.WriteString(`,"duplicate_companies":`)
			w.val(s.DuplicateCompanies)
			b.WriteString(`,"confidence":`)
			w.float(s.Confidence)
			b.WriteByte('}')
		}
		b.WriteByte(']')
	}
	if r.Scanned != nil {
		fmt.Fprintf(&b, `,"scanned":%d`, *r.Scanned)
	}
	b.WriteByte('}')
	return b.Bytes(), w.err
}

// MarshalJSON renders a MatchResult as Python's compare_leads dict.
func (m MatchResult) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	w := &jw{b: &b}
	b.WriteString(`{"lead_a_id":`)
	w.val(m.LeadAID)
	b.WriteString(`,"lead_b_id":`)
	w.val(m.LeadBID)
	b.WriteString(`,"score":`)
	w.float(m.Score)
	b.WriteString(`,"field_scores":`)
	w.fieldScores(m.FieldScores)
	fmt.Fprintf(&b, `,"is_duplicate":%t}`, m.IsDuplicate)
	return b.Bytes(), w.err
}

// MarshalJSON renders the record in key order.
func (r Rec) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	w := &jw{b: &b}
	b.WriteByte('{')
	for i, k := range r.Keys {
		if i > 0 {
			b.WriteByte(',')
		}
		w.str(k)
		b.WriteByte(':')
		w.val(r.Vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), w.err
}

type jw struct {
	b   *bytes.Buffer
	err error
}

func (w *jw) fieldScores(fs [5]float64) {
	w.b.WriteByte('{')
	for i, name := range fieldNames {
		if i > 0 {
			w.b.WriteByte(',')
		}
		w.b.WriteString(`"` + name + `":`)
		w.float(fs[i])
	}
	w.b.WriteByte('}')
}

func (w *jw) float(f float64) {
	s, err := canonjson.FloatRepr(f)
	if err != nil && w.err == nil {
		w.err = err
	}
	w.b.WriteString(s)
}

func (w *jw) str(s string) {
	if !utf8.ValidString(s) && w.err == nil {
		w.err = errors.New("leaddedup: invalid UTF-8")
	}
	w.b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			w.b.WriteString(`\"`)
		case r == '\\':
			w.b.WriteString(`\\`)
		case r == '\n':
			w.b.WriteString(`\n`)
		case r == '\r':
			w.b.WriteString(`\r`)
		case r == '\t':
			w.b.WriteString(`\t`)
		case r == '\b':
			w.b.WriteString(`\b`)
		case r == '\f':
			w.b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(w.b, `\u%04x`, r)
		default:
			w.b.WriteRune(r)
		}
	}
	w.b.WriteByte('"')
}

func (w *jw) val(v any) {
	switch x := v.(type) {
	case nil:
		w.b.WriteString("null")
	case bool:
		w.b.WriteString(strconv.FormatBool(x))
	case int:
		w.b.WriteString(strconv.Itoa(x))
	case int32:
		w.b.WriteString(strconv.Itoa(int(x)))
	case int64:
		w.b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		w.float(x)
	case string:
		w.str(x)
	case []any:
		w.b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				w.b.WriteByte(',')
			}
			w.val(e)
		}
		w.b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				w.b.WriteByte(',')
			}
			w.str(k)
			w.b.WriteByte(':')
			w.val(x[k])
		}
		w.b.WriteByte('}')
	default:
		if w.err == nil {
			w.err = fmt.Errorf("leaddedup: unsupported value type %T", v)
		}
	}
}

func roundDecimal(x float64, n int) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	return f
}
