package firmo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// testdata/scavio_maps_search.json is a live /api/v2/google/maps/search
// response for "Epoch Coffee Austin", trimmed to two places (thumbnails and
// extensions dropped, no values altered).

const scavioTestKey = "sk_test_scavio_0001"

func scavioFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/scavio_maps_search.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// scavioServe answers every request with status and body, recording the requests.
func scavioServe(status int, body string, seen *[]*http.Request, bodies *[]string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		*seen = append(*seen, r)
		*bodies = append(*bodies, string(b))
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
}

func scavioKey(k string) func() (string, error) { return func() (string, error) { return k, nil } }

func scavioRun(t *testing.T, status int, body string, lead map[string]any) (providers.Output, []*http.Request, []string) {
	t.Helper()
	var seen []*http.Request
	var bodies []string
	p, _ := providers.Get("scavio_maps")
	out, err := p.Enrich(context.Background(), scavioServe(status, body, &seen, &bodies), scavioKey(scavioTestKey),
		providers.Input{Lead: lead, Field: "phone"})
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	return out, seen, bodies
}

func fieldsJSON(t *testing.T, out providers.Output) string {
	t.Helper()
	if out.Extra == nil {
		return "{}"
	}
	b, err := out.Extra.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScavioMapsHitFromCapturedResponse(t *testing.T) {
	raw, err := os.ReadFile("testdata/scavio_maps_search.json")
	if err != nil {
		t.Fatal(err)
	}
	out, seen, bodies := scavioRun(t, 200, string(raw), map[string]any{"company": "Epoch Coffee", "city": "Austin"})
	if len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}
	r := seen[0]
	if r.Method != http.MethodPost || r.URL.String() != "https://api.scavio.dev/api/v2/google/maps/search" {
		t.Errorf("request %s %s", r.Method, r.URL)
	}
	if r.Header.Get("Authorization") != "Bearer "+scavioTestKey || r.Header.Get("Content-Type") != "application/json" || len(r.Header) != 2 {
		t.Errorf("headers %v", sortedHeaderNames(r.Header))
	}
	if bodies[0] != `{"query":"Epoch Coffee Austin"}` {
		t.Errorf("body %s", bodies[0])
	}
	want := `{"phone":"(512) 454-3762","address":"221 W N Loop Blvd, Austin, TX 78751",` +
		`"description":"Hip coffee shop with light bites. Cool, vibrant small-chain cafe featuring nibbles, tea & carefully brewed espresso drinks.",` +
		`"industry_tags":"Coffee shop","google_rating":"4.5/5 (2534 reviews)","google_maps_url":"https://maps.google.com/?cid=140078896924485689"}`
	if got := fieldsJSON(t, out); got != want {
		t.Errorf("fields\n got %s\nwant %s", got, want)
	}
	if out.Error != "" || out.Confidence != 0.90 || out.Value != "(512) 454-3762" || out.CostUSD != 0 {
		t.Errorf("out = %+v", out)
	}
}

func sortedHeaderNames(h http.Header) []string {
	m := map[string]string{}
	for k := range h {
		m[k] = ""
	}
	return sortedKeys(m)
}

func TestScavioMapsCompanyOnlyQueryAndJSONEscaping(t *testing.T) {
	_, _, bodies := scavioRun(t, 200, `{"local_results":[]}`, map[string]any{"company": "Joe's \"Best\" Café\n"})
	if len(bodies) != 1 || bodies[0] != `{"query":"Joe's \"Best\" Café\n"}` {
		t.Errorf("body %q", bodies)
	}
}

func TestScavioMapsFallbackFields(t *testing.T) {
	place := scavioFixture(t)["local_results"].([]any)[0].(map[string]any)
	delete(place, "type")
	delete(place, "data_cid")
	delete(place, "reviews")
	place["rating"] = int64(5)
	place["description"] = strings.Repeat("é", 600)
	body, _ := json.Marshal(map[string]any{"local_results": []any{place}})
	out, _, _ := scavioRun(t, 200, string(body), map[string]any{"company": "Epoch Coffee"})
	var f map[string]string
	if err := json.Unmarshal([]byte(fieldsJSON(t, out)), &f); err != nil {
		t.Fatal(err)
	}
	if f["industry_tags"] != "Coffee shop" || f["google_rating"] != "5/5 (0 reviews)" ||
		f["google_maps_url"] != "https://www.google.com/maps/place/?q=place_id:ChIJG-gJw2vKRIYROWi2uwOp8QE" ||
		f["description"] != strings.Repeat("é", 500) {
		t.Errorf("fields %v", f)
	}
}

func TestScavioMapsPicksTheFirstPlaceNamedLikeTheCompany(t *testing.T) {
	body := `{"local_results":[{"title":"Austin Plumbing Supply","phone":"1"},{"title":"ACME Plumbing & Drain","phone":"2"},` +
		`{"title":"Acme Plumbing","phone":"3"}]}`
	out, _, _ := scavioRun(t, 200, body, map[string]any{"company": "The Acme Plumbing Co.", "city": "Austin"})
	if out.Value != "2" {
		t.Errorf("picked %v, want the second place", out.Value)
	}
	// only the first three places are considered
	body = `{"local_results":[{"title":"A"},{"title":"B"},{"title":"C"},{"title":"Acme"}]}`
	if out, _, _ := scavioRun(t, 200, body, map[string]any{"company": "Acme"}); out.Error != "No matching place found" {
		t.Errorf("fourth place used: %+v", out)
	}
}

func TestScavioMapsNameMatching(t *testing.T) {
	for _, tc := range []struct {
		company, title string
		want           bool
	}{
		{"Epoch Coffee", "Epoch Coffee - Village", true},
		{"Café Olé LLC", "Cafe Ole", true},
		{"McDonald's", "McDonald's", true},
		{"Joe's Pizza", "Joe's Pizza Broadway", true},
		{"Inc.", "Anything", false},
		{"Acme Plumbing", "Acme Dental", false},
		{"Zxqv Nonexistent Widgets", "Whales of Iceland", false},
		{"Blue Bottle Coffee", "Starbucks Coffee", false},
		{"Acme", "", false},
	} {
		if got := sameBusiness(tc.company, tc.title); got != tc.want {
			t.Errorf("sameBusiness(%q, %q) = %v", tc.company, tc.title, got)
		}
	}
}

func TestScavioMapsSkipsUnrelatedBusinessWithSharedName(t *testing.T) {
	body := `{"local_results":[{"title":"Acme Dental","phone":"wrong"},{"title":"Acme Plumbing","phone":"correct"}]}`
	out, _, _ := scavioRun(t, 200, body, map[string]any{"company": "Acme Plumbing", "city": "Austin"})
	if out.Value != "correct" {
		t.Fatalf("got phone %v, want correct matching business", out.Value)
	}
}

func TestScavioMapsMissesAndErrors(t *testing.T) {
	lead := map[string]any{"company": "Acme Plumbing", "city": "Austin"}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", 401, `{"error":"invalid api key"}`, "Scavio API error: 401"},
		{"rate limited", 429, `{}`, "Scavio API error: 429"},
		{"server error", 500, `oops`, "Scavio API error: 500"},
		{"empty list", 200, `{"local_results":[]}`, "No places found"},
		{"no list", 200, `{"search_parameters":{}}`, "No places found"},
		{"wrong list type", 200, `{"local_results":{"a":1}}`, "No places found"},
		{"non-object place", 200, `{"local_results":["x"]}`, "No matching place found"},
		{"other business", 200, `{"local_results":[{"title":"Whales of Iceland","phone":"+354 571 0077"}]}`, "No matching place found"},
		{"top-level array", 200, `[]`, "No places found"},
		{"nothing useful", 200, `{"local_results":[{"title":"Acme Plumbing","rating":0,"phone":7}]}`, "No useful data from Scavio Maps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, seen, _ := scavioRun(t, tc.status, tc.body, lead)
			if len(seen) != 1 || out.Error != tc.want || out.Extra != nil {
				t.Errorf("out = %+v (requests %d), want error %q", out, len(seen), tc.want)
			}
		})
	}
	out, _, _ := scavioRun(t, 200, `{"local_results": [`, lead)
	if out.Error == "" || out.Extra != nil {
		t.Errorf("malformed JSON: %+v", out)
	}
}

func TestScavioMapsNoRequestWithoutKeyOrCompany(t *testing.T) {
	p, _ := providers.Get("scavio_maps")
	never := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("request made")
		return nil, errors.New("no network")
	})}
	out, err := p.Enrich(context.Background(), never, scavioKey(""), inputOf(map[string]any{"company": "Acme"}))
	if err != nil || out.Error != "No Scavio API key configured" {
		t.Errorf("no key: %+v %v", out, err)
	}
	out, err = p.Enrich(context.Background(), never, scavioKey(scavioTestKey), inputOf(map[string]any{"city": "Austin"}))
	if err != nil || out.Error != "No company name available" {
		t.Errorf("no company: %+v %v", out, err)
	}
	boom := errors.New("credentials: transport error")
	out, err = p.Enrich(context.Background(), never, func() (string, error) { return "", boom }, inputOf(map[string]any{"company": "Acme"}))
	if !errors.Is(err, boom) || out.Error != "" {
		t.Errorf("key lookup failure: %+v %v", out, err)
	}
}

func TestScavioMapsTransportErrorNeverCarriesKey(t *testing.T) {
	p, _ := providers.Get("scavio_maps")
	fail := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial " + r.URL.String() + " " + r.Header.Get("Authorization") + " refused")
	})}
	out, err := p.Enrich(context.Background(), fail, scavioKey(scavioTestKey), inputOf(map[string]any{"company": "Acme"}))
	if err != nil || out.Error == "" || strings.Contains(out.Error, scavioTestKey) {
		t.Errorf("out = %+v err = %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Enrich(ctx, fail, scavioKey(scavioTestKey), inputOf(map[string]any{"company": "Acme"})); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: %v", err)
	}
}

func TestScavioMapsRegistered(t *testing.T) {
	p, ok := providers.Get("scavio_maps")
	if !ok || p.Name() != "scavio_maps" {
		t.Fatal("scavio_maps not registered under its own name")
	}
	if k, ok := p.(providers.Keyed); !ok || len(k.KeyEnv()) != 1 || k.KeyEnv()[0] != "SCAVIO_API_KEY" {
		t.Errorf("KeyEnv = %v", k)
	}
}
