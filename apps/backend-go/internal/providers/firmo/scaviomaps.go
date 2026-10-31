package firmo

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const scavioMapsURL = "https://api.scavio.dev/api/v2/google/maps/search"

// scavioMaps looks a local business up on Google Maps through the Scavio API
// (https://scavio.dev/docs). It is Go-native (no Python module) and mirrors
// google_maps: same "<company> <city>" query, same fields and confidence, so
// either can fill the same waterfall slot. Maps answers an unknown name with
// whatever is nearby, so of the first three places the first whose title
// shares the company's leading name word wins; none is a miss.
type scavioMaps struct{}

func (scavioMaps) Name() string     { return "scavio_maps" }
func (scavioMaps) KeyEnv() []string { return []string{"SCAVIO_API_KEY"} }

func (p scavioMaps) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, err := key()
	if err != nil {
		return providers.Output{}, err
	}
	if k == "" {
		return fail("No Scavio API key configured"), nil
	}
	company := lead(in, "company")
	if company == "" {
		return fail("No company name available"), nil
	}
	query := company
	if city := lead(in, "city"); city != "" {
		query += " " + city
	}
	out, err := p.run(ctx, c, k, query, in)
	return finish(ctx, out, err, 200)
}

func (scavioMaps) run(ctx context.Context, c *http.Client, key, query string, in providers.Input) (providers.Output, error) {
	resp, err := providers.Do(ctx, c, providers.Req{Method: http.MethodPost, URL: scavioMapsURL,
		Header: [][2]string{{"Content-Type", "application/json"}, {"Authorization", "Bearer " + key}},
		Body:   []byte(`{"query":` + jsonStr(query) + `}`)}, key)
	if err != nil {
		return providers.Output{}, err
	}
	if resp.Status != 200 {
		return fail(fmt.Sprintf("Scavio API error: %d", resp.Status)), nil
	}
	data, err := providers.ParseJSON(resp.Body)
	if err != nil {
		return providers.Output{}, err
	}
	var places []any
	if m, ok := data.(map[string]any); ok {
		places, _ = m["local_results"].([]any)
	}
	if len(places) == 0 {
		return fail("No places found"), nil
	}
	var place map[string]any
	for _, v := range places[:min(len(places), 3)] {
		if m, ok := v.(map[string]any); ok {
			if title, _ := m["title"].(string); sameBusiness(lead(in, "company"), title) {
				place = m
				break
			}
		}
	}
	if place == nil {
		return fail("No matching place found"), nil
	}
	text := func(k string) string { s, _ := place[k].(string); return s }

	f := newFields()
	if v := text("phone"); v != "" {
		f.Set("phone", v)
	}
	if v := text("address"); v != "" {
		f.Set("address", v)
	}
	if v := text("description"); v != "" {
		f.Set("description", providers.TruncRunes(v, 500))
	}
	industry := text("type")
	if types, ok := place["types"].([]any); ok && industry == "" && len(types) > 0 {
		industry, _ = types[0].(string)
	}
	if industry != "" {
		f.Set("industry_tags", industry)
	}
	if rating, ok := place["rating"]; ok && providers.Truthy(rating) {
		if _, num := providers.Num(rating); num {
			reviews := any(int64(0))
			if providers.Truthy(place["reviews"]) {
				reviews = place["reviews"]
			}
			f.Set("google_rating", pyStr(rating)+"/5 ("+pyStr(reviews)+" reviews)")
		}
	}
	if cid := text("data_cid"); cid != "" {
		f.Set("google_maps_url", "https://maps.google.com/?cid="+cid)
	} else if id := text("place_id"); id != "" {
		f.Set("google_maps_url", "https://www.google.com/maps/place/?q=place_id:"+id)
	}
	if f.Len() > 0 {
		return success(in, f, 0.90), nil
	}
	return fail("No useful data from Scavio Maps"), nil
}

// nameStopwords are legal suffixes and fillers that do not identify a business.
var nameStopwords = map[string]bool{"the": true, "and": true, "of": true, "inc": true, "llc": true, "ltd": true,
	"limited": true, "corp": true, "corporation": true, "co": true, "company": true, "gmbh": true, "ag": true,
	"sa": true, "plc": true, "llp": true, "pvt": true, "pte": true}

// nameTokens lowercases s, strips accents and splits it into letter/digit
// words, dropping stopwords and one-character words.
func nameTokens(s string) []string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(strings.ToLower(s)) {
		if norm.NFD.PropertiesString(string(r)).CCC() == 0 {
			b.WriteRune(r)
		}
	}
	var out []string
	for _, w := range strings.FieldsFunc(b.String(), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		if len([]rune(w)) > 1 && !nameStopwords[w] {
			out = append(out, w)
		}
	}
	return out
}

// sameBusiness reports whether the place title contains the company's first
// significant word (a name with none is not checked).
func sameBusiness(company, title string) bool {
	want := nameTokens(company)
	if len(want) == 0 {
		return true
	}
	for _, w := range nameTokens(title) {
		if w == want[0] {
			return true
		}
	}
	return false
}
