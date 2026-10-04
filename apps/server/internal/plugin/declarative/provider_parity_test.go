package declarative

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

type providerScenario struct {
	Name     string            `json:"name"`
	Manifest string            `json:"manifest"`
	Inputs   map[string]any    `json:"inputs"`
	Env      map[string]string `json:"env"`
	Response *struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
		Raw    *string         `json:"raw"`
	} `json:"response"`
	Result struct {
		Success    bool    `json:"success"`
		Fields     string  `json:"fields"`
		Confidence float64 `json:"confidence"`
		Error      *string `json:"error"`
	} `json:"result"`
	Request *struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestProviderParityWithPython replays scenarios recorded from the Python
// DeclarativeProvider (testdata/gen_provider_parity.py) and compares the
// outgoing request and the result.
func TestProviderParityWithPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/provider_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []providerScenario
	if err := json.Unmarshal(raw, &scenarios); err != nil {
		t.Fatal(err)
	}
	if len(scenarios) < 20 {
		t.Fatalf("too few scenarios: %d", len(scenarios))
	}
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			p, err := manifest.LoadBytes(sc.Name+".yaml", []byte(sc.Manifest))
			if err != nil {
				t.Fatal(err)
			}
			var got *http.Request
			var gotBody []byte
			client, err := egress.New(egress.Options{DefaultRPS: 1000, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				got = r
				if r.Body != nil {
					gotBody, _ = io.ReadAll(r.Body)
				}
				resp := &http.Response{StatusCode: sc.Response.Status, Header: http.Header{}}
				if sc.Response.Raw != nil {
					resp.Header.Set("Content-Type", "text/plain")
					resp.Body = io.NopCloser(strings.NewReader(*sc.Response.Raw))
				} else {
					resp.Header.Set("Content-Type", "application/json")
					resp.Body = io.NopCloser(strings.NewReader(string(sc.Response.Body)))
				}
				return resp, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			res, err := RunProvider(context.Background(), p, sc.Inputs, SecretMap(sc.Env), client)
			if err != nil {
				t.Fatal(err)
			}
			wantErr := ""
			if sc.Result.Error != nil {
				wantErr = *sc.Result.Error
			}
			if res.Success != sc.Result.Success || res.Error != wantErr || res.Confidence != sc.Result.Confidence {
				t.Fatalf("result: go={%v %q %v} py={%v %q %v}", res.Success, res.Error, res.Confidence, sc.Result.Success, wantErr, sc.Result.Confidence)
			}
			fields := res.Fields
			if fields == nil {
				fields = pycompat.NewMap()
			}
			if c, _ := pycompat.Canonical(fields); string(c) != sc.Result.Fields {
				t.Fatalf("fields: go=%s py=%s", c, sc.Result.Fields)
			}
			if sc.Request == nil {
				if got != nil {
					t.Fatalf("python sent no request, go sent %s", got.URL)
				}
				return
			}
			if got == nil {
				t.Fatal("go sent no request")
			}
			if got.Method != sc.Request.Method || got.URL.String() != sc.Request.URL {
				t.Fatalf("request line: go=%s %s py=%s %s", got.Method, got.URL, sc.Request.Method, sc.Request.URL)
			}
			if string(gotBody) != sc.Request.Body {
				t.Fatalf("body:\n go=%s\n py=%s", gotBody, sc.Request.Body)
			}
			gotHeaders := map[string]string{}
			for k := range got.Header {
				lk := strings.ToLower(k)
				if lk == "user-agent" || lk == "content-length" {
					continue
				}
				gotHeaders[lk] = got.Header.Get(k)
			}
			if len(gotHeaders) != len(sc.Request.Headers) {
				t.Fatalf("headers: go=%v py=%v", gotHeaders, sc.Request.Headers)
			}
			for k, v := range sc.Request.Headers {
				if gotHeaders[k] != v {
					t.Fatalf("header %s: go=%q py=%q", k, gotHeaders[k], v)
				}
			}
		})
	}
}
