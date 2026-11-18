package contact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

type ceTransport func(*http.Request) (*http.Response, error)

func (f ceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func ceResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCompanyEnrichGoldenRequestsAndMappings(t *testing.T) {
	cases := []struct {
		action, field, method, path, query, body, response string
		lead                                               map[string]any
		want                                               any
	}{
		{"company", "company_size", "GET", "/companies/enrich", "domain=acme.example&waitForEnrichment=false", "", `{"id":"00000000-0000-0000-0000-000000000001","name":"Acme","employees":"501-1K","industries":["Software"],"technologies":[],"location":{"city":{"name":"Austin"}},"financial":{"funding_stage":"series-a"}}`, map[string]any{"website": "https://www.acme.example/products"}, "501-1000"},
		{"company", "description", "POST", "/companies/enrich", "", `{"name":"Acme"}`, `{"id":"c1","description":"Software for teams"}`, map[string]any{"company": "Acme"}, "Software for teams"},
		{"person", "contact_title", "POST", "/people/lookup", "", `{"email":"ada@acme.example"}`, `{"id":9223372036854775807,"name":"Ada Example","position":"Engineer","socials":{"linkedin_url":"https://linkedin.com/in/ada-example"}}`, map[string]any{"email": "ada@acme.example"}, "Engineer"},
		{"workforce", "companyenrich_workforce", "GET", "/companies/workforce", "domain=acme.example", "", `{"company_id":"c1","domain":"acme.example","observed_employee_count":0,"department_headcount":{},"history":[]}`, map[string]any{"website": "acme.example"}, nil},
		{"people", "companyenrich_people", "POST", "/people/search", "", `{"domains":["acme.example"],"page":1,"pageSize":10}`, `{"items":[{"id":9223372036854775807,"name":"Ada"}],"totalItems":1,"page":1,"totalPages":1}`, map[string]any{"website": "acme.example"}, nil},
		{"search", "companyenrich_search", "POST", "/companies/search", "", `{"semanticQuery":"B2B payroll software","page":1,"pageSize":10}`, `{"items":[],"totalItems":0,"page":1,"totalPages":0}`, map[string]any{"description": "B2B payroll software"}, nil},
		{"similar", "companyenrich_similar", "POST", "/companies/similar", "waitForEnrichment=false", `{"domains":["acme.example"],"page":1,"pageSize":10}`, `{"items":[{"id":"c2","name":"Example"}],"metadata":{"scores":{"c2":0}},"totalItems":1}`, map[string]any{"website": "acme.example"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.action+tc.method, func(t *testing.T) {
			calls := 0
			c := &http.Client{Transport: ceTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Scheme != "https" || r.URL.Host != "api.companyenrich.com" || r.Method != tc.method || r.URL.Path != tc.path || r.URL.RawQuery != tc.query {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer "+testKey {
					t.Fatal("missing bearer credential")
				}
				raw := []byte{}
				if r.Body != nil {
					raw, _ = io.ReadAll(r.Body)
				}
				if string(raw) != tc.body {
					t.Fatalf("body %s, want %s", raw, tc.body)
				}
				return ceResponse(200, tc.response), nil
			})}
			out, err := (companyEnrich{tc.action}).Enrich(context.Background(), c, keyOf(testKey), providers.Input{Field: tc.field, Lead: tc.lead})
			if err != nil || out.Error != "" || calls != 1 {
				t.Fatalf("out=%+v err=%v calls=%d", out, err, calls)
			}
			if tc.want != nil && out.Value != tc.want {
				t.Fatalf("value=%v want=%v", out.Value, tc.want)
			}
			raw, exists := out.Extra.Get("companyenrich_" + tc.action)
			if !exists {
				t.Fatal("full response missing")
			}
			var source, actual any
			sourceDecoder := json.NewDecoder(strings.NewReader(tc.response))
			sourceDecoder.UseNumber()
			_ = sourceDecoder.Decode(&source)
			actualDecoder := json.NewDecoder(strings.NewReader(raw.(string)))
			actualDecoder.UseNumber()
			_ = actualDecoder.Decode(&actual)
			a, _ := json.Marshal(source)
			b, _ := json.Marshal(actual)
			if string(a) != string(b) {
				t.Fatalf("response lost data: %s != %s", a, b)
			}
			if tc.action == "people" || tc.action == "person" {
				if !strings.Contains(raw.(string), "9223372036854775807") {
					t.Fatal("int64 ID lost precision")
				}
			}
			if _, exists := out.Extra.Get("email"); exists {
				t.Fatal("profile lookup must not claim it found an email")
			}
		})
	}
}

func TestCompanyEnrichRefusesBeforeNetwork(t *testing.T) {
	c := &http.Client{Transport: ceTransport(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected paid call"); return nil, nil })}
	for _, action := range []string{"company", "person", "workforce", "people", "search", "similar", "unknown"} {
		out, err := (companyEnrich{action}).Enrich(context.Background(), c, keyOf(testKey), providers.Input{})
		if err != nil || out.Error == "" {
			t.Errorf("%s accepted missing input: %+v %v", action, out, err)
		}
	}
	bad, _ := (companyEnrich{"company"}).Enrich(context.Background(), c, keyOf(testKey), providers.Input{Field: "email", Lead: map[string]any{"website": "acme.example"}})
	if bad.Error == "" {
		t.Fatal("unsupported field accepted")
	}
	out, err := (companyEnrich{"company"}).Enrich(context.Background(), c, keyOf(""), providers.Input{Lead: map[string]any{"website": "acme.example"}})
	if err != nil || !strings.Contains(out.Error, "No CompanyEnrich API key") {
		t.Fatalf("missing key: %+v %v", out, err)
	}
	sentinel := errors.New("credential lookup failed")
	_, err = (companyEnrich{"company"}).Enrich(context.Background(), c, func() (string, error) { return "", sentinel }, providers.Input{Lead: map[string]any{"website": "acme.example"}})
	if !errors.Is(err, sentinel) {
		t.Fatal("credential error must remain infrastructure failure")
	}
}

func TestCompanyEnrichErrorsAreSafeAndNotRetried(t *testing.T) {
	for _, status := range []int{301, 400, 401, 402, 404, 422, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			c := &http.Client{Transport: ceTransport(func(*http.Request) (*http.Response, error) {
				calls++
				r := ceResponse(status, testKey+" confidential body")
				r.Header.Set("Location", "https://other.example")
				return r, nil
			})}
			out, err := (companyEnrich{"company"}).Enrich(context.Background(), c, keyOf(testKey), providers.Input{Lead: map[string]any{"website": "acme.example"}})
			if err != nil || out.Error == "" || strings.Contains(out.Error, testKey) || strings.Contains(out.Error, "confidential") || calls != 1 {
				t.Fatalf("unsafe/retried failure: %+v %v calls=%d", out, err, calls)
			}
		})
	}
	for _, body := range []string{`[]`, `null`, `{`, `{"description":"no identity"}`} {
		c := &http.Client{Transport: ceTransport(func(*http.Request) (*http.Response, error) { return ceResponse(200, body), nil })}
		out, _ := (companyEnrich{"company"}).Enrich(context.Background(), c, keyOf(testKey), providers.Input{Lead: map[string]any{"website": "acme.example"}})
		if out.Error == "" {
			t.Fatalf("malformed success accepted: %s", body)
		}
	}
}

func TestCompanyEnrichTransportKeepsDialerAndDoesNotReuseConnections(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "api.companyenrich.com" {
			t.Errorf("host changed: %s", r.Host)
		}
		_, _ = w.Write([]byte(`{"id":"company-1","description":"example"}`))
	}))
	defer srv.Close()
	c := srv.Client()
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	tr.TLSClientConfig.ServerName = "example.com" // httptest certificate; production host is unchanged
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		if addr != "api.companyenrich.com:443" {
			t.Errorf("guarded target changed: %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	for range 2 {
		out, err := (companyEnrich{"company"}).Enrich(context.Background(), c, keyOf(testKey), providers.Input{Field: "description", Lead: map[string]any{"website": "acme.example"}})
		if err != nil || out.Error != "" {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	}
	if dials.Load() != 2 {
		t.Fatalf("expected a fresh guarded connection per request, got %d", dials.Load())
	}
	if tr.DisableKeepAlives {
		t.Fatal("shared transport was mutated")
	}
}
