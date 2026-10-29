package certification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

type vectors map[string]any

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/certification.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return vectors(v.(map[string]any))
}

func (v vectors) list(k string) []map[string]any {
	var out []map[string]any
	for _, e := range v[k].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, strings.Replace(s, "+00:00", "Z", 1))
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// clearEnv keeps tests hermetic: empty Options fields read the environment.
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv(PathEnv, "")
	t.Setenv(KeyEnv, "")
	t.Setenv(BuildSHAEnv, "")
}

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := canonjson.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func asStrings(v any) []string {
	var out []string
	for _, e := range v.([]any) {
		out = append(out, e.(string))
	}
	return out
}

// optionsFor writes the vector's certificate file and returns Options.
func optionsFor(t *testing.T, c map[string]any) Options {
	c = withDefaults(t, c)
	t.Helper()
	dir := t.TempDir()
	path := ""
	switch f := c["certificates_file"].(type) {
	case nil:
		if c["file"] != "no_path" {
			path = filepath.Join(dir, "does-not-exist.json")
		}
	case string:
		path = filepath.Join(dir, "c.json")
		if err := os.WriteFile(path, []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		b, err := canonjson.Marshal(f, false)
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, "c.json")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := mustTime(t, c["now"].(string))
	return Options{Path: path, Key: c["key"].(string), BuildSHA: c["build_sha"].(string), Now: func() time.Time { return now }}
}

func withDefaults(t *testing.T, c map[string]any) map[string]any {
	top := loadVectors(t)
	out := map[string]any{}
	for k, v := range c {
		out[k] = v
	}
	for _, k := range []string{"key", "now", "build_sha"} {
		if _, ok := out[k]; !ok {
			out[k] = top[k]
		}
	}
	return out
}

func TestStatusVectors(t *testing.T) {
	clearEnv(t)
	v := loadVectors(t)
	for _, c := range v.list("catalogs") {
		name := c["file"].(string) + "/" + c["kind"].(string)
		t.Run(name, func(t *testing.T) {
			o := optionsFor(t, c)
			var got any
			switch c["kind"] {
			case "integration":
				got = IntegrationCatalog(o)
			case "signal":
				got = SignalSourceCatalog(o)
			case "agent":
				got = AgentCapabilityCatalog(o)
			case "audience":
				got = AudienceCapabilityCatalog(o)
			case "governance":
				got = GovernanceCapabilityCatalog(o)
			default:
				t.Fatalf("unknown kind %v", c["kind"])
			}
			if !reflect.DeepEqual(roundTrip(t, got), c["catalog"]) {
				t.Fatalf("catalog mismatch\n got %s\nwant %v", mustJSON(got), c["catalog"])
			}
		})
	}
	for _, c := range v.list("statuses") {
		t.Run("statuses/"+c["name"].(string), func(t *testing.T) {
			o := optionsFor(t, c)
			var builds map[string]string
			if m, ok := c["subject_builds"].(map[string]any); ok {
				builds = map[string]string{}
				for k, s := range m {
					builds[k] = s.(string)
				}
			}
			got := StatusFor(asStrings(c["subjects"]), builds, o)
			if _, raises := c["statuses"].(map[string]any)["error"]; raises {
				// Python raises ValueError; Go omits unknown subjects (documented deviation).
				for _, id := range asStrings(c["subjects"]) {
					if _, present := got[id]; present && !Known(id) {
						t.Fatalf("unknown %q present", id)
					}
				}
				return
			}
			if !reflect.DeepEqual(roundTrip(t, got), c["statuses"]) {
				t.Fatalf("statuses mismatch\n got %s\nwant %v", mustJSON(got), c["statuses"])
			}
		})
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestCatalogKeyOrderMatchesPython(t *testing.T) {
	clearEnv(t)
	b, _ := json.Marshal(IntegrationCatalog(Options{Path: "none"}))
	want := `{"id":"webhook","category":"activation","capabilities":["outbound"],"required_checks":["authentication","external_write","idempotency","retry_recovery","tenant_isolation"],"maturity":"beta","certification":null}`
	if !strings.HasPrefix(string(b), "["+want+",") {
		t.Fatalf("got %s", b)
	}
	b, _ = json.Marshal(SignalSourceCatalog(Options{Path: "none"}))
	if !strings.HasPrefix(string(b), `[{"id":"jobspy","signal_types":["hiring","partnership_hiring"],"required_checks":`) {
		t.Fatalf("got %s", b)
	}
}

func TestAttestMatchesPythonBytes(t *testing.T) {
	v := loadVectors(t)
	n := 0
	for _, a := range v.list("attest") {
		t.Run(a["name"].(string), func(t *testing.T) {
			got, err := Attest(a["certificate"].(map[string]any), a["key"].(string))
			if want, bad := a["error"].(string); bad {
				if err == nil || err.Error() != want {
					t.Fatalf("err = %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, a["value"]) {
				t.Fatalf("got %v\nwant %v", got, a["value"])
			}
		})
		n++
	}
	if n < 70 {
		t.Fatalf("only %d attest vectors", n)
	}
}

func TestValidVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.list("valid") {
		t.Run(c["name"].(string), func(t *testing.T) {
			got := valid(c["certificate"].(map[string]any), c["key"].(string), mustTime(t, c["now"].(string)), c["build_sha"].(string))
			if got != c["valid"].(bool) {
				t.Fatalf("valid = %v, want %v", got, c["valid"])
			}
		})
	}
}

func TestParseTimeVectors(t *testing.T) {
	v := loadVectors(t)
	for _, p := range v.list("parse_time") {
		got, ok := parseTime(p["value"])
		want, has := p["parsed"].(string)
		if ok != has {
			t.Fatalf("%v: ok=%v want %v", p["value"], ok, has)
		}
		if has && !got.Equal(mustTime(t, want)) {
			t.Fatalf("%v: got %v want %v", p["value"], got, want)
		}
	}
}

func TestValidBySubjectVectors(t *testing.T) {
	v := loadVectors(t)
	now := mustTime(t, v["now"].(string))
	for _, c := range v.list("valid_by_subject") {
		t.Run(c["name"].(string), func(t *testing.T) {
			var requested map[string]bool
			if r, ok := c["requested"].([]any); ok {
				requested = map[string]bool{}
				for _, s := range r {
					requested[s.(string)] = true
				}
			}
			var builds map[string]string
			if m, ok := c["subject_builds"].(map[string]any); ok {
				builds = map[string]string{}
				for k, s := range m {
					builds[k] = s.(string)
				}
			}
			got := validBySubject(c["certificates"].([]any), v["key"].(string), now, v["build_sha"].(string), requested, builds)
			var ids []string
			runs := map[string]any{}
			for id, cert := range got {
				ids = append(ids, id)
				runs[id] = cert["validation_run_id"]
			}
			sort.Strings(ids)
			want := asStrings(c["valid_subjects"])
			sort.Strings(want)
			if len(ids) != len(want) || (len(ids) > 0 && !reflect.DeepEqual(ids, want)) {
				t.Fatalf("subjects %v, want %v", ids, want)
			}
			if !reflect.DeepEqual(runs, c["valid_run_ids"]) {
				t.Fatalf("runs %v, want %v", runs, c["valid_run_ids"])
			}
		})
	}
}

func TestRequiredChecksPerSubject(t *testing.T) {
	v := loadVectors(t)
	for _, r := range v.list("required_checks") {
		id := r["subject"].(string)
		if _, raises := r["error"]; raises {
			if Known(id) || RequiredChecks(id) != nil {
				t.Fatalf("%q must be unknown", id)
			}
			continue
		}
		if got, want := RequiredChecks(id), asStrings(r["value"]); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v want %v", id, got, want)
		}
		if !Known(id) {
			t.Fatalf("%s should be known", id)
		}
	}
}

// fixture builds one attested hubspot certificate valid on 2026-09-01.
func fixture(t *testing.T, mutate func(map[string]any)) (cert map[string]any, key string) {
	t.Helper()
	v := loadVectors(t)
	key = v["key"].(string)
	var base map[string]any
	for _, c := range v.list("valid") {
		if c["name"] == "valid" {
			base = c["certificate"].(map[string]any)
		}
	}
	c := map[string]any{}
	for k, x := range base {
		c[k] = x
	}
	delete(c, "attestation")
	if mutate != nil {
		mutate(c)
	}
	return c, key
}

func statusAt(t *testing.T, c map[string]any, key, now string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.json")
	b, _ := canonjson.Marshal([]any{c}, true)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	clearEnv(t)
	tm := mustTime(t, now)
	for _, e := range IntegrationCatalog(Options{Path: path, Key: key, BuildSHA: "vector-build-0001", Now: func() time.Time { return tm }}) {
		if e.ID == "hubspot" {
			return e.Maturity
		}
	}
	return ""
}

func TestExpiredAndFutureCertificateRejected(t *testing.T) {
	c, key := fixture(t, nil)
	att, err := Attest(c, key)
	if err != nil {
		t.Fatal(err)
	}
	for now, want := range map[string]string{
		"2026-08-19T23:59:59Z": "beta", "2026-08-20T00:00:00Z": "supported",
		"2026-10-31T23:59:59Z": "supported", "2026-11-01T00:00:00Z": "beta", "2027-01-01T00:00:00Z": "beta",
	} {
		if got := statusAt(t, att, key, now); got != want {
			t.Errorf("now=%s: %s, want %s", now, got, want)
		}
	}
}

func TestBuildShaMismatchRejected(t *testing.T) {
	c, key := fixture(t, nil)
	att, _ := Attest(c, key)
	path := filepath.Join(t.TempDir(), "c.json")
	b, _ := canonjson.Marshal([]any{att}, true)
	_ = os.WriteFile(path, b, 0o600)
	clearEnv(t)
	now := mustTime(t, "2026-09-01T12:00:00Z")
	for _, build := range []string{"other-build", "VECTOR-BUILD-0001"} {
		for _, e := range IntegrationCatalog(Options{Path: path, Key: key, BuildSHA: build, Now: func() time.Time { return now }}) {
			if e.ID == "hubspot" && e.Maturity != "beta" {
				t.Fatalf("build %q accepted", build)
			}
		}
	}
	// empty BuildSHA falls back to the env, which is unset: still beta
	for _, e := range IntegrationCatalog(Options{Path: path, Key: key, Now: func() time.Time { return now }}) {
		if e.ID == "hubspot" && e.Maturity != "beta" {
			t.Fatal("empty build accepted")
		}
	}
	t.Setenv(BuildSHAEnv, "vector-build-0001")
	for _, e := range IntegrationCatalog(Options{Path: path, Key: key, Now: func() time.Time { return now }}) {
		if e.ID == "hubspot" && e.Maturity != "supported" {
			t.Fatal("env build sha not read")
		}
	}
}

func TestForgedSignatureRejected(t *testing.T) {
	c, key := fixture(t, nil)
	att, _ := Attest(c, key)
	tamper := func(f func(a map[string]any, att map[string]any)) map[string]any {
		cp := map[string]any{}
		for k, v := range att {
			cp[k] = v
		}
		a := map[string]any{}
		for k, v := range att["attestation"].(map[string]any) {
			a[k] = v
		}
		f(cp, a)
		cp["attestation"] = a
		return cp
	}
	cases := map[string]map[string]any{
		"evidence_url": tamper(func(c, a map[string]any) { c["evidence_url"] = "https://evil.example.com/x" }),
		"signature":    tamper(func(c, a map[string]any) { a["signature"] = strings.Repeat("0", 64) }),
		"upper":        tamper(func(c, a map[string]any) { a["signature"] = strings.ToUpper(a["signature"].(string)) }),
		"key_id":       tamper(func(c, a map[string]any) { a["key_id"] = "0000000000000000" }),
		"algorithm":    tamper(func(c, a map[string]any) { a["algorithm"] = "hmac-sha512" }),
	}
	now := mustTime(t, "2026-09-01T12:00:00Z")
	if !valid(att, key, now, "vector-build-0001") {
		t.Fatal("control certificate must be valid")
	}
	for name, cert := range cases {
		if valid(cert, key, now, "vector-build-0001") {
			t.Errorf("%s: forged certificate accepted", name)
		}
	}
	if valid(att, key+"x", now, "vector-build-0001") || valid(att, "", now, "vector-build-0001") {
		t.Error("wrong or empty key accepted")
	}
}

func TestLifetimeOver92DaysRejected(t *testing.T) {
	for exp, ok := range map[string]bool{"2026-11-20T00:00:00Z": true, "2026-11-21T00:00:00Z": false, "2026-08-20T00:00:00Z": false, "2026-08-19T00:00:00Z": false} {
		// 2026-08-20 + 92 days = 2026-11-20
		c, key := fixture(t, func(c map[string]any) { c["expires_at"] = exp })
		_, err := Attest(c, key)
		if (err == nil) != ok {
			t.Errorf("expires %s: err=%v want ok=%v", exp, err, ok)
		}
	}
	// a signed over-long certificate (signature computed directly) is still rejected on read
	c, key := fixture(t, func(c map[string]any) { c["expires_at"] = "2027-06-01T00:00:00Z" })
	body, _ := payload(c)
	c["attestation"] = map[string]any{"algorithm": "hmac-sha256", "key_id": keyID(key), "signature": sign(key, body)}
	if valid(c, key, mustTime(t, "2026-09-01T12:00:00Z"), "vector-build-0001") {
		t.Fatal("over-long certificate accepted")
	}
}

func TestUnknownSubjectRejected(t *testing.T) {
	c, key := fixture(t, func(c map[string]any) { c["subject_id"] = "nope" })
	if _, err := Attest(c, key); err == nil || err.Error() != "certificate subject is unknown" {
		t.Fatalf("err = %v", err)
	}
	for _, id := range []string{"nope", "", "connector:", "connector:Bad", "provider:1x", "connector:" + strings.Repeat("a", 81), "x connector:a"} {
		if Known(id) || RequiredChecks(id) != nil {
			t.Errorf("%q must be unknown", id)
		}
	}
	if !Known("connector:"+strings.Repeat("a", 80)) || !Known("provider:a-b_9") {
		t.Fatal("valid dynamic subjects rejected")
	}
	clearEnv(t)
	got := StatusFor([]string{"connector:Bad-Name", "hubspot"}, nil, Options{Path: "none"})
	if _, present := got["connector:Bad-Name"]; present || got["hubspot"].Maturity != "beta" {
		t.Fatalf("unknown subject must be omitted, got %v", got)
	}
}

func TestMissingFileIsUncertified(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{nope"), 0o600)
	for _, p := range []string{"", filepath.Join(dir, "missing.json"), dir, bad} {
		for _, e := range IntegrationCatalog(Options{Path: p, Key: "k", BuildSHA: "b"}) {
			if e.Maturity != "beta" || e.Certification != nil {
				t.Fatalf("path %q: %s not beta", p, e.ID)
			}
		}
	}
	s := StatusFor([]string{"connector:a"}, map[string]string{"connector:a": strings.Repeat("a", 64)}, Options{Path: bad})
	if s["connector:a"].Maturity != "beta" {
		t.Fatal("connector not beta")
	}
}

func TestIntegrationCatalogMatchesVector(t *testing.T) {
	clearEnv(t)
	v := loadVectors(t)
	seen := 0
	for _, c := range v.list("catalogs") {
		if c["file"] != "all_families" || c["kind"] != "integration" {
			continue
		}
		seen++
		got := IntegrationCatalog(optionsFor(t, c))
		if !reflect.DeepEqual(roundTrip(t, got), c["catalog"]) {
			t.Fatal("all_families integration catalog differs")
		}
	}
	if seen != 1 {
		t.Fatalf("vector missing (%d)", seen)
	}
	if n := len(IntegrationCatalog(Options{Path: "none"})); n != 12 {
		t.Fatalf("%d integrations", n)
	}
}

func TestKeyNeverInOutput(t *testing.T) {
	clearEnv(t)
	const key = "ctk_super_secret_never_print_0001"
	c, _ := fixture(t, nil)
	att, err := Attest(c, key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustJSON(att), key) {
		t.Fatal("key in attested certificate")
	}
	bad, _ := fixture(t, func(c map[string]any) { c["status"] = "nope" })
	if _, err := Attest(bad, key); err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("error leaks key: %v", err)
	}
	if _, err := Attest(c, ""); err == nil || strings.Contains(err.Error(), key) {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "c.json")
	b, _ := canonjson.Marshal([]any{att}, true)
	_ = os.WriteFile(path, b, 0o600)
	now := mustTime(t, "2026-09-01T12:00:00Z")
	o := Options{Path: path, Key: key, BuildSHA: "vector-build-0001", Now: func() time.Time { return now }}
	for _, out := range []string{mustJSON(IntegrationCatalog(o)), mustJSON(StatusFor([]string{"hubspot"}, nil, o)), mustJSON(AgentCapabilityCatalog(o))} {
		if strings.Contains(out, key) || strings.Contains(out, "attestation") {
			t.Fatal("key or attestation in catalog output")
		}
	}
}

func TestEnvFallback(t *testing.T) {
	c, key := fixture(t, nil)
	att, _ := Attest(c, key)
	path := filepath.Join(t.TempDir(), "c.json")
	b, _ := canonjson.Marshal([]any{att}, true)
	_ = os.WriteFile(path, b, 0o600)
	t.Setenv(PathEnv, path)
	t.Setenv(KeyEnv, key)
	t.Setenv(BuildSHAEnv, "vector-build-0001")
	now := mustTime(t, "2026-09-01T12:00:00Z")
	for _, e := range IntegrationCatalog(Options{Now: func() time.Time { return now }}) {
		if e.ID == "hubspot" && e.Maturity != "supported" {
			t.Fatal("env path/key/build not honoured")
		}
	}
}
