package destvalidate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func decodeNum(t *testing.T, raw []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// norm round-trips v through JSON so numbers and maps compare structurally.
func norm(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	decodeNum(t, b, &out)
	return out
}

func TestValidateConfigMatchesVectors(t *testing.T) {
	var f struct {
		Cases []struct {
			Type   string         `json:"type"`
			Config map[string]any `json:"config"`
			Ok     bool           `json:"ok"`
			Error  string         `json:"error"`
			Result map[string]any `json:"result"`
			Label  string         `json:"label"`
		} `json:"cases"`
	}
	decodeNum(t, readFile(t, "testdata/config.json"), &f)
	if len(f.Cases) < 120 {
		t.Fatalf("only %d vectors", len(f.Cases))
	}
	for i, c := range f.Cases {
		got, err := ValidateConfig(c.Type, c.Config)
		if c.Ok {
			if err != nil {
				t.Errorf("case %d (%s %s): unexpected error %v", i, c.Type, c.Label, err)
				continue
			}
			if !reflect.DeepEqual(norm(t, got), norm(t, c.Result)) {
				t.Errorf("case %d (%s %s): result %v want %v", i, c.Type, c.Label, got, c.Result)
			}
			continue
		}
		if err == nil || err.Error() != c.Error {
			t.Errorf("case %d (%s %s): error %v want %q", i, c.Type, c.Label, err, c.Error)
		}
	}
}

func TestTypes(t *testing.T) {
	if len(Types) != 12 {
		t.Fatalf("Types = %v", Types)
	}
	for _, ty := range []string{"webhook", "slack", "airtable", "google_sheets", "meta_ads"} {
		if !IsType(ty) {
			t.Errorf("IsType(%q) false", ty)
		}
	}
	if IsType("carrier_pigeon") || IsType("") {
		t.Error("IsType accepted junk")
	}
}

func TestCredentialKeysRejectedAtAnyDepth(t *testing.T) {
	const want = "credentials must use workspace secrets, not destination config"
	for _, raw := range []string{
		`{"token":"x"}`, `{"A":{"B":{"API_KEY":1}}}`, `{"l":[{"x":1},[{"Password":"p"}]]}`,
		`{"url":"https://h.example/","nested":{"secret":""}}`,
	} {
		var cfg map[string]any
		decodeNum(t, []byte(raw), &cfg)
		for _, ty := range []string{"webhook", "hubspot", "unknown_type"} {
			if _, err := ValidateConfig(ty, cfg); err == nil || err.Error() != want {
				t.Errorf("%s %s: err %v", ty, raw, err)
			}
		}
	}
	for _, raw := range []string{`{"tokens":1}`, `{"note":"token"}`, `{"x-token":{"my_secret":1}}`} {
		var cfg map[string]any
		decodeNum(t, []byte(raw), &cfg)
		if _, err := ValidateConfig("hubspot", cfg); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestAdIDsRejectPathInjection(t *testing.T) {
	base := func(dt string) map[string]any {
		c := map[string]any{"consent_attested": true, "consent_source": "s"}
		switch dt {
		case "meta_ads":
			c["custom_audience_id"] = "1"
		case "google_ads":
			c["customer_id"] = "1"
			c["user_list_id"] = "2"
		case "linkedin_ads":
			c["segment_id"] = "3"
		}
		return c
	}
	bad := []string{"../x", "1/2", "1?x=1", "1#f", "1%2f2", "a b", " 1", "1\n", "..", ".", strings.Repeat("9", 65), "1;2", "é"}
	keys := map[string][]string{
		"meta_ads":     {"custom_audience_id", "login_customer_id", "api_version"},
		"google_ads":   {"customer_id", "user_list_id", "login_customer_id", "api_version"},
		"linkedin_ads": {"segment_id", "login_customer_id", "api_version"},
	}
	for dt, ks := range keys {
		for _, k := range ks {
			for _, v := range bad {
				cfg := base(dt)
				cfg[k] = v
				_, err := ValidateConfig(dt, cfg)
				if err == nil || err.Error() != "invalid ad destination config: "+k {
					t.Errorf("%s %s=%q: err %v", dt, k, v, err)
				}
			}
		}
	}
	// api_version patterns: vNN, vNN.N, six digits.
	for _, v := range []string{"v1", "v23", "v23.0", "202607"} {
		cfg := base("meta_ads")
		cfg["api_version"] = v
		if _, err := ValidateConfig("meta_ads", cfg); err != nil {
			t.Errorf("api_version %q: %v", v, err)
		}
	}
	for _, v := range []string{"23", "v", "v123", "v1.23", "2026071", "x1", "v1.0.0"} {
		cfg := base("meta_ads")
		cfg["api_version"] = v
		if _, err := ValidateConfig("meta_ads", cfg); err == nil {
			t.Errorf("api_version %q accepted", v)
		}
	}
	// wrong JSON types for an id are rejected, numbers accepted.
	cfg := base("meta_ads")
	cfg["custom_audience_id"] = []any{"1"}
	if _, err := ValidateConfig("meta_ads", cfg); err == nil {
		t.Error("list id accepted")
	}
	cfg = base("meta_ads")
	cfg["custom_audience_id"] = json.Number("123")
	if _, err := ValidateConfig("meta_ads", cfg); err != nil {
		t.Errorf("numeric id: %v", err)
	}
	// Non-ad destinations are not subject to the ad id rules.
	if _, err := ValidateConfig("hubspot", map[string]any{"customer_id": "../x"}); err != nil {
		t.Errorf("hubspot: %v", err)
	}
}

type fixture struct {
	Request struct {
		Body json.RawMessage `json:"body"`
	} `json:"request"`
	Response struct {
		Status int `json:"status"`
		Body   struct {
			Detail json.RawMessage `json:"detail"`
		} `json:"body"`
	} `json:"response"`
}

func errsAsJSON(t *testing.T, err error) any {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a *ValidationError", err)
	}
	b, jerr := json.Marshal(ve.Errors)
	if jerr != nil {
		t.Fatal(jerr)
	}
	var out any
	decodeNum(t, b, &out)
	return out
}

// bodyFixtures yields the recorded 422 list-details whose loc starts with body.
func bodyFixtures(t *testing.T, glob string) map[string]fixture {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join("../../testdata/contract/destinations", glob))
	out := map[string]fixture{}
	for _, p := range files {
		var fx fixture
		decodeNum(t, readFile(t, p), &fx)
		if fx.Response.Status != 422 || len(fx.Request.Body) == 0 || fx.Request.Body[0] != '{' {
			continue
		}
		var detail []map[string]any
		if json.Unmarshal(fx.Response.Body.Detail, &detail) != nil || len(detail) == 0 {
			continue
		}
		if loc, _ := detail[0]["loc"].([]any); len(loc) == 0 || loc[0] != "body" {
			continue
		}
		out[filepath.Base(p)] = fx
	}
	return out
}

func compareDetail(t *testing.T, name string, fx fixture, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error", name)
		return
	}
	var want any
	decodeNum(t, fx.Response.Body.Detail, &want)
	if got := errsAsJSON(t, err); !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		t.Errorf("%s:\n got  %.600s\n want %.600s", name, gb, wb)
	}
}

func TestDecodeCreateErrorsMatchFastAPI(t *testing.T) {
	fixtures := bodyFixtures(t, "dest_create_*_422.json")
	if len(fixtures) < 8 {
		t.Fatalf("only %d create fixtures", len(fixtures))
	}
	for name, fx := range fixtures {
		_, err := DecodeCreate(fx.Request.Body)
		compareDetail(t, name, fx, err)
	}

	var m struct {
		Create []struct {
			Name   string          `json:"name"`
			Body   json.RawMessage `json:"body"`
			Create struct {
				Ok     map[string]any `json:"ok"`
				Errors []struct {
					Type string   `json:"type"`
					Loc  []string `json:"loc"`
					Msg  string   `json:"msg"`
				} `json:"errors"`
			} `json:"create"`
			Validated struct {
				Ok     bool           `json:"ok"`
				Config map[string]any `json:"config"`
				Error  string         `json:"error"`
			} `json:"validated_config"`
		} `json:"create"`
	}
	decodeNum(t, readFile(t, "testdata/models.json"), &m)
	for _, c := range m.Create {
		got, err := DecodeCreate(c.Body)
		if c.Create.Ok == nil {
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Errorf("%s: want validation error, got %v", c.Name, err)
				continue
			}
			if len(ve.Errors) != len(c.Create.Errors) {
				t.Errorf("%s: %d errors want %d", c.Name, len(ve.Errors), len(c.Create.Errors))
				continue
			}
			for i, e := range c.Create.Errors {
				g := ve.Errors[i]
				loc := make([]string, 0, len(g.Loc))
				for _, p := range g.Loc[1:] {
					loc = append(loc, p.(string))
				}
				if g.Loc[0] != "body" || g.Type != e.Type || g.Msg != e.Msg || !reflect.DeepEqual(loc, e.Loc) {
					t.Errorf("%s: got %+v want %+v", c.Name, g, e)
				}
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		gotMap := map[string]any{
			"audience_id": got.AudienceID, "name": got.Name, "destination_type": got.DestinationType,
			"enabled": got.Enabled, "config": got.Config, "field_map": got.FieldMap,
		}
		if !reflect.DeepEqual(norm(t, gotMap), norm(t, c.Create.Ok)) {
			t.Errorf("%s: got %v want %v", c.Name, gotMap, c.Create.Ok)
		}
		cfg, verr := ValidateConfig(got.DestinationType, got.Config)
		if c.Validated.Ok {
			if verr != nil || !reflect.DeepEqual(norm(t, cfg), norm(t, c.Validated.Config)) {
				t.Errorf("%s: validated %v %v", c.Name, cfg, verr)
			}
		} else if verr == nil || verr.Error() != c.Validated.Error {
			t.Errorf("%s: validate err %v want %q", c.Name, verr, c.Validated.Error)
		}
	}
}

func TestDecodeCreateKeepsKeyOrderAndAllErrors(t *testing.T) {
	c, err := DecodeCreate([]byte(`{"audience_id":"a","name":"n","destination_type":"slack","config":{"z":1,"a":2,"m":{"q":1}},"field_map":{"b":"1","a":"2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.ConfigKeys, []string{"z", "a", "m"}) || !reflect.DeepEqual(c.FieldMapKeys, []string{"b", "a"}) {
		t.Errorf("keys %v %v", c.ConfigKeys, c.FieldMapKeys)
	}
	// Several bad fields are all reported, in model field order.
	_, err = DecodeCreate([]byte(`{"name":" ","destination_type":"x","enabled":"maybe","config":[]}`))
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Errors) != 5 {
		t.Fatalf("err %v", err)
	}
	var locs []string
	for _, e := range ve.Errors {
		locs = append(locs, e.Loc[1].(string))
	}
	if !reflect.DeepEqual(locs, []string{"audience_id", "name", "destination_type", "enabled", "config"}) {
		t.Errorf("order %v", locs)
	}
	// Not an object / not JSON / empty.
	for _, raw := range []string{`[1]`, `"x"`, `null`} {
		_, err = DecodeCreate([]byte(raw))
		if !errors.As(err, &ve) || ve.Errors[0].Type != "model_attributes_type" || len(ve.Errors) != 1 {
			t.Errorf("%s: %v", raw, err)
		}
	}
	_, err = DecodeCreate(nil)
	if !errors.As(err, &ve) || ve.Errors[0].Type != "missing" {
		t.Errorf("empty: %v", err)
	}
	_, err = DecodeCreate([]byte(`{`))
	if !errors.As(err, &ve) || ve.Errors[0].Type != "json_invalid" {
		t.Errorf("bad json: %v", err)
	}
}

func TestDecodePatchExcludeUnset(t *testing.T) {
	for name, fx := range bodyFixtures(t, "dest_patch_*_422.json") {
		_, err := DecodePatch(fx.Request.Body)
		compareDetail(t, name, fx, err)
	}
	var m struct {
		Patch []struct {
			Name   string          `json:"name"`
			Body   json.RawMessage `json:"body"`
			Ok     map[string]any  `json:"ok"`
			Errors []struct {
				Type string   `json:"type"`
				Loc  []string `json:"loc"`
				Msg  string   `json:"msg"`
			} `json:"errors"`
		} `json:"patch"`
	}
	decodeNum(t, readFile(t, "testdata/models.json"), &m)
	if len(m.Patch) < 10 {
		t.Fatal("too few patch vectors")
	}
	for _, c := range m.Patch {
		p, err := DecodePatch(c.Body)
		if c.Ok == nil {
			var ve *ValidationError
			if !errors.As(err, &ve) || len(ve.Errors) != len(c.Errors) {
				t.Errorf("%s: %v", c.Name, err)
				continue
			}
			for i, e := range c.Errors {
				g := ve.Errors[i]
				if g.Type != e.Type || g.Msg != e.Msg || g.Loc[0] != "body" || g.Loc[1] != e.Loc[0] {
					t.Errorf("%s: got %+v want %+v", c.Name, g, e)
				}
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if !reflect.DeepEqual(norm(t, p.Dump()), norm(t, c.Ok)) {
			t.Errorf("%s: dump %v want %v", c.Name, p.Dump(), c.Ok)
		}
	}
	// A present null is distinct from an absent key.
	p, err := DecodePatch([]byte(`{"name":null}`))
	if err != nil || !p.Name.Set || !p.Name.Null || p.Enabled.Set || p.Config.Set || p.FieldMap.Set {
		t.Errorf("null patch %+v %v", p, err)
	}
	p, err = DecodePatch([]byte(`{"enabled":false,"name":" x ","config":{"b":1,"a":2}}`))
	if err != nil || p.Name.Val != "x" || p.Enabled.Null || p.Enabled.Val || !p.Enabled.Set ||
		!reflect.DeepEqual(p.ConfigKeys, []string{"b", "a"}) {
		t.Errorf("patch %+v %v", p, err)
	}
}

func TestDecodeInboundBounds(t *testing.T) {
	fixtures := bodyFixtures(t, "cb_invalid_*_422.json")
	if len(fixtures) < 9 {
		t.Fatalf("only %d inbound fixtures", len(fixtures))
	}
	for name, fx := range fixtures {
		_, err := DecodeInbound(fx.Request.Body)
		compareDetail(t, name, fx, err)
	}
	// Boundaries that must pass.
	ok := []string{
		`{"external_event_id":"` + strings.Repeat("e", 255) + `"}`,
		`{"external_event_id":"e","external_record_id":"` + strings.Repeat("r", 255) + `"}`,
		`{"external_event_id":"e","external_record_id":null,"lead_id":null}`,
		`{"external_event_id":"e","lead_id":"7"}`,
		`{"external_event_id":"e","lead_id":7.0}`,
		`{"external_event_id":"e","fields":{"a":null,"b":1,"c":2.5,"d":true,"e":"` + strings.Repeat("x", 10000) + `"}}`,
		`{"external_event_id":"e","fields":{"e":"` + strings.Repeat("é", 10000) + `"}}`,
	}
	for _, raw := range ok {
		if _, err := DecodeInbound([]byte(raw)); err != nil {
			t.Errorf("%.80s: %v", raw, err)
		}
	}
	in, err := DecodeInbound([]byte(`{"external_event_id":"e","lead_id":"7","fields":{"z":1,"a":"x"}}`))
	if err != nil || in.LeadID == nil || *in.LeadID != 7 || !reflect.DeepEqual(in.FieldKeys, []string{"z", "a"}) {
		t.Errorf("inbound %+v %v", in, err)
	}
	bad := []string{
		`{"external_event_id":"e","lead_id":1.5}`,
		`{"external_event_id":"e","fields":{"e":"` + strings.Repeat("x", 10001) + `"}}`,
		`{"external_event_id":"e","fields":null}`,
		`{"external_event_id":null}`,
		`{"external_event_id":5}`,
	}
	for _, raw := range bad {
		if _, err := DecodeInbound([]byte(raw)); err == nil {
			t.Errorf("%.80s accepted", raw)
		}
	}
}

func TestCursorRoundTripAndErrors(t *testing.T) {
	ts, id, err := DecodeHistoryCursor("WyIyMDI2LTA4LTI4VDEwOjAwOjAwIiwicmNwLWh1Yi01Il0", "inbound receipt")
	if err != nil || id != "rcp-hub-5" || !ts.Equal(time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %v %v", ts, id, err)
	}
	if got := EncodeHistoryCursor(ts, id); got != "WyIyMDI2LTA4LTI4VDEwOjAwOjAwIiwicmNwLWh1Yi01Il0" {
		t.Errorf("encode %s", got)
	}
	if got := EncodeHistoryCursor(time.Date(2026, 8, 27, 10, 0, 0, 123456000, time.UTC), "run-1"); got != encodeRaw(`["2026-08-27T10:00:00.123456","run-1"]`) {
		t.Errorf("micro %s", got)
	}
	// Non-UTC input is converted to the naive UTC wall clock (DB timestamps).
	loc := time.FixedZone("x", 2*3600)
	if got := EncodeHistoryCursor(time.Date(2026, 8, 27, 12, 0, 0, 0, loc), "r"); got != encodeRaw(`["2026-08-27T10:00:00","r"]`) {
		t.Errorf("tz %s", got)
	}
	for _, v := range []struct {
		raw  string
		want time.Time
	}{
		{`["2026-08-27T10:00:00+02:00","r"]`, time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)},
		{`["2026-08-27T10:00:00Z","r"]`, time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)},
		{`["2026-08-27 10:00:00.5","r"]`, time.Date(2026, 8, 27, 10, 0, 0, 500000000, time.UTC)},
		{`["2026-08-27","r"]`, time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)},
	} {
		got, _, err := DecodeHistoryCursor(encodeRaw(v.raw), "x")
		if err != nil || !got.Equal(v.want) {
			t.Errorf("%s: %v %v", v.raw, got, err)
		}
	}
	for _, c := range []string{
		"not-base64!!", "_w", "WyIyMDI2LTA4LTI3VDEwOjAwOjAwIl0", "WyIyMDI2LTA4LTI3VDEwOjAwOjAwIiwieCIsInkiXQ",
		"eyJhIjoxfQ", "", encodeRaw(`["nope","r"]`), encodeRaw(`[5,"r"]`), encodeRaw(`["2026-08-27T10:00:00",5]`),
		encodeRaw(`["2026-13-40T10:00:00","r"]`),
	} {
		_, _, err := DecodeHistoryCursor(c, "destination run")
		if err == nil || err.Error() != "invalid destination run cursor" {
			t.Errorf("%q: err %v", c, err)
		}
	}
}

func TestRenderSlackMatchesVectors(t *testing.T) {
	var f struct {
		Cases []struct {
			Template string         `json:"template"`
			Snapshot map[string]any `json:"snapshot"`
			Text     *string        `json:"text"`
			Raises   *string        `json:"raises"`
		} `json:"cases"`
	}
	decodeNum(t, readFile(t, "../destdeliver/testdata/slack_format.json"), &f)
	ok := 0
	for _, c := range f.Cases {
		fields := map[string]any{}
		for k, v := range c.Snapshot {
			if v == nil {
				fields[k] = ""
			} else {
				fields[k] = colgraph.PyStr(v)
			}
		}
		got, err := RenderSlackTemplate(c.Template, fields)
		if err != nil {
			if !errors.Is(err, ErrInvalidTemplate) || err.Error() != "invalid slack message_template" {
				t.Errorf("%q: error %v", c.Template, err)
			}
			if c.Raises == nil && !strings.ContainsAny(c.Template, "!:.[") {
				t.Errorf("%q: rejected but Python renders it", c.Template)
			}
			if cerr := CheckSlackTemplate(c.Template); cerr == nil {
				t.Errorf("%q: Render fails but CheckSlackTemplate passes", c.Template)
			}
			continue
		}
		if c.Raises != nil {
			t.Errorf("%q: Python raises %s but Go rendered %q", c.Template, *c.Raises, got)
			continue
		}
		if r := []rune(got); len(r) > 3000 {
			got = string(r[:3000])
		}
		if c.Text == nil || got != *c.Text {
			t.Errorf("%q: got %q want %v", c.Template, got, c.Text)
		}
		ok++
	}
	if ok < 25 {
		t.Errorf("only %d vectors rendered", ok)
	}
}

func TestSlackUnsupportedFieldSyntaxRejected(t *testing.T) {
	const msg = "slack message_template uses unsupported fields"
	for _, tpl := range []string{
		"{company.__class__}", "{company[0]}", "{company!r}", "{company:>10}", "{company:}",
		"{0}", "{}", "{12}", "{a", "}", "{", "x}", "{{{a", "{a{b}}",
	} {
		_, err := ValidateConfig("slack", map[string]any{"webhook_secret_ref": "R", "message_template": tpl})
		if err == nil || err.Error() != msg {
			t.Errorf("%q: %v", tpl, err)
		}
		if _, err := RenderSlackTemplate(tpl, map[string]any{"company": "Acme"}); !errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("render %q: %v", tpl, err)
		}
	}
	for _, tpl := range []string{"{company}", "{{x}} {a_b} {A-B} { spaced }", "plain", ""} {
		if _, err := ValidateConfig("slack", map[string]any{"webhook_secret_ref": "R", "message_template": tpl}); err != nil {
			t.Errorf("%q: %v", tpl, err)
		}
	}
	// The 2000 length check (code points) comes first.
	long := strings.Repeat("{x.y}", 401)
	if _, err := ValidateConfig("slack", map[string]any{"webhook_secret_ref": "R", "message_template": long}); err == nil ||
		err.Error() != "slack message_template may not exceed 2000 characters" {
		t.Errorf("long: %v", err)
	}
	got, err := RenderSlackTemplate("{x} {y} {z}", map[string]any{"x": "1", "y": nil, "w": 5})
	if err != nil || got != "1" {
		t.Errorf("render %q %v", got, err)
	}
	got, _ = RenderSlackTemplate("", map[string]any{"company": "Acme", "contact_person": "Ada", "email": "e@x"})
	if got != "New audience member: Acme Ada e@x" {
		t.Errorf("default %q", got)
	}
}

func TestNameTrimAndBlank(t *testing.T) {
	c, err := DecodeCreate([]byte(`{"audience_id":"a","name":"  x y  ","destination_type":"WebHook"}`))
	if err != nil || c.Name != "x y" || c.DestinationType != "webhook" || !c.Enabled {
		t.Fatalf("%+v %v", c, err)
	}
	for _, name := range []string{"   ", "\t\n", "\u00a0"} {
		b, _ := json.Marshal(map[string]any{"audience_id": "a", "name": name, "destination_type": "webhook"})
		_, err := DecodeCreate(b)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Errors[0].Msg != "Value error, name cannot be blank" {
			t.Errorf("%q: %v", name, err)
		}
	}
	var _ httpx.FieldError
}

func encodeRaw(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
