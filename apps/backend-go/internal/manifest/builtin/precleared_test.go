package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest/builtin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// The Precleared fixtures pin the request the manifest sends and the result it projects for each
// outcome the vendor contract defines. They are hand-written from https://precleared.dev/openapi.json
// with example.com-style data only, and replayed in process: CI makes no network or paid request.

const preclearedName = "precleared_compliance"

type preclearedFixture struct {
	Scenario    string            `json:"scenario"`
	Lead        map[string]string `json:"lead"`
	Credentials map[string]string `json:"credentials"`
	Request     struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response struct {
		Status           int               `json:"status"`
		Headers          map[string]string `json:"headers"`
		Body             string            `json:"body"`
		TransportError   string            `json:"transport_error"`
		TransportTimeout string            `json:"transport_timeout"`
	} `json:"response"`
	Result struct {
		Success    bool           `json:"success"`
		Error      string         `json:"error"`
		HTTPStatus int            `json:"http_status"`
		Confidence float64        `json:"confidence"`
		Fields     map[string]any `json:"fields"`
	} `json:"result"`
	file string
}

func preclearedManifest(t *testing.T) manifest.Manifest {
	t.Helper()
	dir, trust, err := builtin.Paths()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := manifest.LoadIndex(dir, "optional", manifest.LoadTrustStore(trust))
	if err != nil {
		t.Fatal(err)
	}
	e, ok := idx[preclearedName]
	if !ok {
		t.Fatalf("built-in index lacks %s", preclearedName)
	}
	return e.Manifest
}

func loadPreclearedFixtures(t *testing.T) []*preclearedFixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "precleared", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Precleared fixtures")
	}
	var out []*preclearedFixture
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fx := &preclearedFixture{file: f}
		if err := json.Unmarshal(raw, fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, fx)
	}
	return out
}

type transportTimeout struct{ msg string }

func (e transportTimeout) Error() string   { return e.msg }
func (e transportTimeout) Timeout() bool   { return true }
func (e transportTimeout) Temporary() bool { return true }

// replayOne serves one fixture exchange and records every deviation from the pinned request.
type replayOne struct {
	t     *testing.T
	fx    *preclearedFixture
	calls int
}

func (r *replayOne) secret(s string) string {
	for k, v := range r.fx.Credentials {
		s = strings.ReplaceAll(s, "{{secret:"+k+"}}", v)
	}
	return s
}

func (r *replayOne) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	want := r.fx.Request
	if req.Method != want.Method {
		r.t.Errorf("%s: method %s want %s", r.fx.file, req.Method, want.Method)
	}
	if got := req.URL.String(); got != want.URL {
		r.t.Errorf("%s: url %s want %s", r.fx.file, got, want.URL)
	}
	got := map[string]string{}
	for k, v := range req.Header {
		got[strings.ToLower(k)] = strings.Join(v, ",")
	}
	wantH := map[string]string{}
	for k, v := range want.Headers {
		wantH[strings.ToLower(k)] = r.secret(v)
	}
	if !reflect.DeepEqual(got, wantH) {
		r.t.Errorf("%s: headers differ: got names %v want %v", r.fx.file, keys(got), keys(wantH))
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	if string(body) != want.Body {
		r.t.Errorf("%s: body\n got %s\nwant %s", r.fx.file, body, want.Body)
	}
	resp := r.fx.Response
	switch {
	case resp.TransportTimeout != "":
		return nil, transportTimeout{msg: resp.TransportTimeout}
	case resp.TransportError != "":
		return nil, errors.New(resp.TransportError)
	}
	h := http.Header{}
	for k, v := range resp.Headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: resp.Status, Status: http.StatusText(resp.Status), Header: h,
		Body: io.NopCloser(strings.NewReader(resp.Body)), Request: req}, nil
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fixtureCreds resolves only what the fixture configures; anything else is unset, as for a
// workspace that never saved it.
func fixtureCreds(c map[string]string) func(string) (string, error) {
	return func(name string) (string, error) { return c[name], nil }
}

func TestPreclearedFixtures(t *testing.T) {
	m := preclearedManifest(t)
	seen := map[string]bool{}
	for _, fx := range loadPreclearedFixtures(t) {
		t.Run(fx.Scenario, func(t *testing.T) {
			rt := &replayOne{t: t, fx: fx}
			res := manifest.Execute(context.Background(), &http.Client{Transport: rt}, m, fx.Lead, fixtureCreds(fx.Credentials))
			if rt.calls != 1 {
				t.Fatalf("%s: %d requests, want 1", fx.file, rt.calls)
			}
			want := fx.Result
			if res.Success != want.Success || res.Error != want.Error || res.HTTPStatus != want.HTTPStatus || res.Confidence != want.Confidence {
				t.Fatalf("%s: result %+v want %+v", fx.file, res, want)
			}
			if len(want.Fields) == 0 {
				if len(res.Fields) != 0 {
					t.Fatalf("%s: fields %v want none", fx.file, res.Fields)
				}
			} else if !reflect.DeepEqual(res.Fields, want.Fields) {
				t.Fatalf("%s: fields\n got %v\nwant %v", fx.file, res.Fields, want.Fields)
			}
			for _, v := range fx.Credentials {
				if v != "" && strings.Contains(res.Error, v) {
					t.Fatalf("%s: error text carries a credential", fx.file)
				}
			}
		})
		seen[fx.Scenario] = true
	}
	// The outcomes the provider contract requires fixtures for.
	for _, s := range []string{"success_ok_to_email", "uncertain_unknown", "invalid_input_no_email", "auth_invalid_key",
		"quota_payment_required", "rate_limited", "network_failure"} {
		if !seen[s] {
			t.Errorf("missing fixture %s", s)
		}
	}
}

// Without a configured key nothing is sent at all: Precleared's keyless free tier is never used.
func TestPreclearedWithoutKeyMakesNoRequest(t *testing.T) {
	m := preclearedManifest(t)
	rt := &replayOne{t: t, fx: &preclearedFixture{}}
	creds := fixtureCreds(map[string]string{"PRECLEARED_SENDER_COUNTRY": "GB", "PRECLEARED_ADDRESS_SOURCE": "published"})
	lead := map[string]string{"email": "jane.smith@example.co.uk", "company": "Example Trading Ltd"}
	res := manifest.Execute(context.Background(), &http.Client{Transport: rt}, m, lead, creds)
	if want := preclearedName + ": missing PRECLEARED_API_KEY"; res.Success || res.Error != want {
		t.Fatalf("result %+v, want error %q", res, want)
	}
	if _, refusal := manifest.Prepare(m, lead, creds, nil); refusal == "" {
		t.Fatal("Prepare allowed a call without a key")
	}
	if res := manifest.Execute(context.Background(), &http.Client{Transport: rt}, m, lead, nil); res.Error == "" {
		t.Fatal("a nil credential resolver allowed a call")
	}
	if rt.calls != 0 {
		t.Fatalf("%d requests without a key", rt.calls)
	}
}

func TestPreclearedManifestContract(t *testing.T) {
	m := preclearedManifest(t)
	if m.Capability != "compliance" || len(m.Capabilities) != 1 || m.Capabilities[0] != "compliance" {
		t.Fatalf("capability %q %v", m.Capability, m.Capabilities)
	}
	if m.Auth.Type != "bearer" || m.Auth.EnvVar != "PRECLEARED_API_KEY" || m.Auth.Value != "" {
		t.Fatalf("auth %+v", m.Auth)
	}
	if p := manifest.SecurityProblem(m); p != "" {
		t.Fatalf("security problem %s", p)
	}
	if m.Request.URL != "https://precleared.dev/api/v1/check" || strings.ToUpper(m.Request.Method) != "POST" {
		t.Fatalf("request %s %s", m.Request.Method, m.Request.URL)
	}
	if m.Request.Timeout <= 0 || m.Request.Timeout > 30 {
		t.Fatalf("timeout %v must be bounded", m.Request.Timeout)
	}
	if m.CostPerLookup <= 0 {
		t.Fatal("cost_per_lookup must be declared")
	}
	// Every setting the manifest can read is workspace-owned, so no workspace is enrolled by an
	// operator-wide value (see secrets.WorkspaceOwned).
	names := manifest.AllowedEnvNames(m)
	if len(names) != 4 {
		t.Fatalf("readable settings %v", names)
	}
	for n := range names {
		if !secrets.WorkspaceOwned[n] {
			t.Errorf("%s is readable by the manifest but not workspace-owned", n)
		}
	}
	// The body carries only what the lead or the workspace states.
	body, ok := m.Request.BodyTemplate.(map[string]any)
	if !ok {
		t.Fatalf("body template %T", m.Request.BodyTemplate)
	}
	var fields []string
	for k := range body {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	if want := []string{"address_source", "company_name", "pitch", "recipient_email", "sender_country"}; !reflect.DeepEqual(fields, want) {
		t.Fatalf("body fields %v want %v", fields, want)
	}
	for _, k := range []string{"compliance", "compliance_rule", "compliance_rule_source", "compliance_next_step",
		"compliance_receipt_id", "compliance_checked_at", "compliance_rules_version", "compliance_disclaimer"} {
		if _, ok := m.Response.Mappings[k]; !ok {
			t.Errorf("mapping %s missing", k)
		}
	}
}
