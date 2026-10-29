package firmo

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	placesURL       = "https://places.googleapis.com/v1/places:searchText"
	placesFieldMask = "places.id,places.displayName,places.formattedAddress," +
		"places.internationalPhoneNumber,places.nationalPhoneNumber," +
		"places.websiteUri,places.googleMapsUri,places.rating," +
		"places.userRatingCount,places.types,places.primaryType," +
		"places.regularOpeningHours,places.editorialSummary," +
		"places.businessStatus"
)

// googleMaps ports providers/google_maps.py.
type googleMaps struct{}

func (googleMaps) Name() string     { return "google_maps" }
func (googleMaps) KeyEnv() []string { return []string{"GOOGLE_MAPS_API_KEY"} }

func (p googleMaps) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, err := key()
	if err != nil {
		return providers.Output{}, err
	}
	if k == "" {
		return fail("No Google Maps API key configured"), nil
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

// jsonStr is json.dumps(s, ensure_ascii=False) (httpx's json= encoding).
func jsonStr(s string) string {
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (p googleMaps) run(ctx context.Context, c *http.Client, key, query string, in providers.Input) (providers.Output, error) {
	body := `{"textQuery":` + jsonStr(query) + `,"maxResultCount":` + strconv.Itoa(3) + `}`
	resp, err := providers.Do(ctx, c, providers.Req{Method: http.MethodPost, URL: placesURL,
		Header: [][2]string{{"Content-Type", "application/json"}, {"X-Goog-Api-Key", key}, {"X-Goog-FieldMask", placesFieldMask}},
		Body:   []byte(body)}, key)
	if err != nil {
		return providers.Output{}, err
	}
	if resp.Status != 200 {
		return fail(fmt.Sprintf("Google API error: %d", resp.Status)), nil
	}
	data, err := providers.ParseJSON(resp.Body)
	if err != nil {
		return providers.Output{}, err
	}
	placesV, err := providers.PyGet(data, "places", []any{})
	if err != nil {
		return providers.Output{}, err
	}
	if !providers.Truthy(placesV) {
		return fail("No places found"), nil
	}
	place, err := indexFirst(placesV)
	if err != nil {
		return providers.Output{}, err
	}
	f := newFields()
	pg := func(k string, def any) (any, error) { return providers.PyGet(place, k, def) }

	phone, err := pg("internationalPhoneNumber", nil)
	if err != nil {
		return providers.Output{}, err
	}
	if !providers.Truthy(phone) {
		if phone, err = pg("nationalPhoneNumber", nil); err != nil {
			return providers.Output{}, err
		}
	}
	if providers.Truthy(phone) {
		f.Set("phone", phone)
	}
	addr, err := pg("formattedAddress", nil)
	if err != nil {
		return providers.Output{}, err
	}
	if providers.Truthy(addr) {
		f.Set("address", addr)
	}
	summary, err := pg("editorialSummary", map[string]any{})
	if err != nil {
		return providers.Output{}, err
	}
	if sm, ok := summary.(map[string]any); ok && providers.Truthy(sm["text"]) {
		t, err := sliceFirst500(sm["text"])
		if err != nil {
			return providers.Output{}, err
		}
		f.Set("description", t)
	}
	primary, err := pg("primaryType", "")
	if err != nil {
		return providers.Output{}, err
	}
	types, err := pg("types", []any{})
	if err != nil {
		return providers.Output{}, err
	}
	if providers.Truthy(primary) {
		s, err := replaceUnderscoreTitle(primary)
		if err != nil {
			return providers.Output{}, err
		}
		f.Set("industry_tags", s)
	} else if providers.Truthy(types) {
		first, err := indexFirst(types)
		if err != nil {
			return providers.Output{}, err
		}
		s, err := replaceUnderscoreTitle(first)
		if err != nil {
			return providers.Output{}, err
		}
		f.Set("industry_tags", s)
	}
	rating, err := pg("rating", nil)
	if err != nil {
		return providers.Output{}, err
	}
	reviews, err := pg("userRatingCount", nil)
	if err != nil {
		return providers.Output{}, err
	}
	if providers.Truthy(rating) {
		rc := any(int64(0))
		if providers.Truthy(reviews) {
			rc = reviews
		}
		f.Set("google_rating", pyStr(rating)+"/5 ("+pyStr(rc)+" reviews)")
	}
	maps, err := pg("googleMapsUri", nil)
	if err != nil {
		return providers.Output{}, err
	}
	if providers.Truthy(maps) {
		f.Set("google_maps_url", maps)
	}
	if f.Len() > 0 {
		return success(in, f, 0.90), nil
	}
	return fail("No useful data from Google Places"), nil
}

// sliceFirst500 is v[:500] for a truthy decoded value.
func sliceFirst500(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return providers.TruncRunes(t, 500), nil
	case []any:
		if len(t) > 500 {
			return t[:500], nil
		}
		return t, nil
	}
	return nil, &providers.PyException{Msg: "'" + providers.TypeName(v) + "' object is not subscriptable"}
}

// replaceUnderscoreTitle is v.replace("_", " ").title().
func replaceUnderscoreTitle(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", &providers.PyException{Msg: "'" + providers.TypeName(v) + "' object has no attribute 'replace'"}
	}
	return pyTitle(strings.ReplaceAll(s, "_", " ")), nil
}
