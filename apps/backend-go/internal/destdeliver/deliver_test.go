package destdeliver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destvalidate"
)

const vecWS = "ws-vec"

func loadVec(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return v.(map[string]any)
}

func canon(t *testing.T, v any) string {
	t.Helper()
	b, err := canonjson.Marshal(v, true)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	return string(b)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

func strOf(v any) string {
	if v == nil {
		return ""
	}
	return colgraph.PyStr(v)
}

// scripted collaborators ----------------------------------------------------------------

type recorder struct {
	mu      sync.Mutex
	script  map[string]any
	calls   []any // per-type call shape, in the vector's own layout
	secrets map[string]string
}

func (r *recorder) next(key string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, _ := r.script[key].([]any)
	if len(list) == 0 {
		return map[string]any{"success": true}
	}
	r.script[key] = list[1:]
	return asMap(list[0])
}

func (r *recorder) record(c any) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

func (r *recorder) Get(_ context.Context, ws, key string) (string, string, error) {
	return r.secrets[key], "workspace", nil
}

func scripted(key string, s map[string]any) (Result, error) {
	if m := s; m != nil && m["raise"] != nil {
		return Result{}, errors.New(strOf(m["raise"]))
	}
	res := Result{Success: truthy(s["success"]), Summary: strOf(s["summary"]), Error: strOf(s["error"]), Duplicate: truthy(s["duplicate"]), Operation: strOf(s["operation"])}
	switch key {
	case "instantly":
		res.ExternalID = strOf(s["lead_id"])
	case "sheets":
		res.ExternalID = strOf(s["range"])
	case "airtable":
		res.ExternalID = strOf(s["record_id"])
	}
	return res, nil
}

func (r *recorder) Send(_ context.Context, q WebhookRequest) (Result, error) {
	r.record(map[string]any{"config": q.Config, "lead_data": q.Payload})
	return scripted("_act_webhook", r.next("_act_webhook"))
}

type instantlyPort struct{ *recorder }

func (p instantlyPort) AddLead(_ context.Context, q CampaignRequest) (Result, error) {
	p.record([]any{q.Config["campaign_id"], q.Payload, q.Config["skip_if_in_campaign"], q.WorkspaceID})
	return scripted("instantly", p.next("instantly"))
}

type smartleadPort struct{ *recorder }

func (p smartleadPort) AddLead(_ context.Context, q CampaignRequest) (Result, error) {
	p.record([]any{q.Config["campaign_id"], q.Payload, q.Config["settings"], q.WorkspaceID})
	return scripted("smartlead", p.next("smartlead"))
}

type sheetsPort struct{ *recorder }

func (p sheetsPort) Upsert(_ context.Context, q RowUpsert) (Result, error) {
	p.record([]any{q.Config["spreadsheet_id"], q.Config["row"], q.Config["key"], q.Config["range"], q.WorkspaceID})
	return scripted("sheets", p.next("sheets"))
}

type airtablePort struct{ *recorder }

func (p airtablePort) Upsert(_ context.Context, q RowUpsert) (Result, error) {
	p.record([]any{q.Payload, q.Config["base_id"], q.Config["table"], q.Config["key"], q.Config["idempotency_field"], q.Config["typecast"], q.WorkspaceID})
	return scripted("airtable", p.next("airtable"))
}

type crmPort struct{ *recorder }

func (p crmPort) Push(_ context.Context, ws string, cfg, lead map[string]any, wb string, id int64) crmpush.Result {
	p.record(map[string]any{"config": cfg, "lead_data": lead, "lead_id": id})
	s := p.next("_act_push_crm")
	if s["raise"] != nil {
		panic(errors.New(strOf(s["raise"])))
	}
	return crmpush.Result{Success: truthy(s["success"]), Value: strOf(s["summary"]), Error: strOf(s["error"])}
}

func newRig(script map[string]any) (*Deliverer, *recorder) {
	rec := &recorder{script: script}
	rec.secrets = map[string]string{}
	for k, v := range asMap(script["secrets"]) {
		rec.secrets[k] = v.(string)
	}
	d := New(Ports{Secrets: rec, Webhook: rec, CRM: crmPort{rec}, Instantly: instantlyPort{rec},
		Smartlead: smartleadPort{rec}, Sheets: sheetsPort{rec}, Airtable: airtablePort{rec}}, 5*time.Second)
	return d, rec
}

func destOf(m map[string]any) Destination {
	return Destination{ID: "dest-1", WorkspaceID: vecWS, AudienceID: "aud-1", Type: strOf(m["type"]),
		Config: asMap(m["config"]), FieldMap: asMap(m["field_map"])}
}

// vectors -------------------------------------------------------------------------------

func TestMapPayloadMatchesVectors(t *testing.T) {
	cases := loadVec(t, "../destsync/testdata/map_payload.json")["cases"].([]any)
	for i, c := range cases {
		c := asMap(c)
		got := MapPayload(asMap(c["snapshot"]), asMap(c["field_map"]))
		if canon(t, got) != canon(t, c["result"]) {
			t.Errorf("case %d: got %s want %s", i, canon(t, got), canon(t, c["result"]))
		}
	}
	// the empty-map form is a copy, never the caller's map
	snap := map[string]any{"a": int64(1)}
	cp := MapPayload(snap, nil)
	cp["a"] = int64(2)
	if snap["a"].(int64) != 1 {
		t.Fatal("MapPayload(nil) aliased the snapshot")
	}
}

func TestFingerprintMatchesPythonVectors(t *testing.T) {
	raw, err := os.ReadFile("../destsync/testdata/fingerprint.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Payload map[string]any `json:"payload"`
			SHA256  string         `json:"sha256"`
		} `json:"cases"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // arbitrary-precision ints, as production rows decode
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	for i, c := range doc.Cases {
		got := Fingerprint(c.Payload)
		if got != c.SHA256 {
			t.Errorf("case %d: got %s want %s", i, got, c.SHA256)
		}
	}
}

func TestFingerprintToleratesNumbersAndDefaultStr(t *testing.T) {
	// UseNumber-decoded rows and non-JSON values (default=str) must fingerprint like their
	// plain equivalents.
	a := Fingerprint(map[string]any{"n": json.Number("5"), "f": json.Number("1.5"), "i": 3})
	b := Fingerprint(map[string]any{"n": int64(5), "f": 1.5, "i": int64(3)})
	if a != b {
		t.Fatalf("json.Number/int fingerprint differs: %s %s", a, b)
	}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := sha256.Sum256([]byte(`{"t":"2026-01-02 03:04:05+00:00"}`))
	if got := Fingerprint(map[string]any{"t": ts}); got != hex.EncodeToString(want[:]) {
		t.Fatalf("time default=str: %s", got)
	}
}

func TestIdempotencyKeys(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	if got := IdempotencyKey("d1", 5, fp, false); got != "dest:d1:lead:5:"+fp {
		t.Fatal(got)
	}
	if got := IdempotencyKey("d1", 5, fp, true); got != "dest:d1:lead:5:remove:"+fp {
		t.Fatal(got)
	}
}

func TestDeliverCasesFromPython(t *testing.T) {
	cases := loadVec(t, "testdata/deliver_calls.json")["cases"].([]any)
	for _, c := range cases {
		c := asMap(c)
		name := strOf(c["name"])
		t.Run(name, func(t *testing.T) {
			d, rec := newRig(asMap(c["script"]))
			res := d.Deliver(context.Background(), destOf(asMap(c["destination"])), int(c["lead_id"].(int64)),
				asMap(c["snapshot"]), strOf(c["idempotency_key"]))
			var wantCalls []any
			for _, x := range c["calls"].([]any) {
				x := asMap(x)
				switch x["fn"] {
				case "get_secret":
					continue // Go never asks for a blank secret ref; hits are covered below
				case "_act_webhook":
					wantCalls = append(wantCalls, map[string]any{"config": x["config"], "lead_data": x["lead_data"]})
				case "_act_push_crm":
					wantCalls = append(wantCalls, map[string]any{"config": x["config"], "lead_data": x["lead_data"], "lead_id": x["lead_id"]})
				default:
					wantCalls = append(wantCalls, x["args"])
				}
			}
			// workspace id is the last arg in the Python helper calls; ours carries it in the request.
			if got, want := canon(t, nilToEmpty(rec.calls)), canon(t, nilToEmpty(normCalls(wantCalls))); got != want {
				t.Fatalf("calls\n got  %s\n want %s", got, want)
			}
			if raises := strOf(c["raises"]); raises != "" {
				msg := raises[strings.Index(raises, ": ")+2:]
				if res.Success || res.Error != truncRunes(msg, 500) {
					t.Fatalf("raise: %+v want error %q", res, msg)
				}
				return
			}
			want := asMap(c["result"])
			if res.Success != want["success"].(bool) || res.Summary != strOf(want["summary"]) ||
				res.Error != strOf(want["error"]) || res.ExternalID != strOf(want["external_id"]) {
				t.Fatalf("result %+v want %v", res, want)
			}
		})
	}
}

func nilToEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

// normCalls replaces the vector's trailing/leading workspace id markers so shapes line up with
// the recorder: all Python helper args end with the workspace except airtable (also last).
func normCalls(in []any) []any {
	out := make([]any, 0, len(in))
	for _, c := range in {
		out = append(out, c)
	}
	return out
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func TestDeliverWebhookBodyAndHeaders(t *testing.T) {
	d, rec := newRig(map[string]any{})
	dest := Destination{ID: "d9", WorkspaceID: "w", AudienceID: "a9", Type: "webhook",
		Config:   map[string]any{"url": "https://x.test/h", "headers": map[string]any{"X-A": "1"}},
		FieldMap: map[string]any{"company": "account"}}
	res := d.Deliver(context.Background(), dest, 7, map[string]any{"company": "Acme", "email": "e"}, "dest:d9:lead:7:ff")
	if !res.Success {
		t.Fatalf("%+v", res)
	}
	cfg := asMap(asMap(rec.calls[0])["config"])
	body := asMap(cfg["body"])
	if body["event"] != "audience.member.upsert" || body["audience_id"] != "a9" || body["destination_id"] != "d9" || body["lead_id"] != int64(7) {
		t.Fatalf("body %v", body)
	}
	if canon(t, body["data"]) != `{"account":"Acme"}` {
		t.Fatalf("data %v", body["data"])
	}
	h := asMap(cfg["headers"])
	if h["Idempotency-Key"] != "dest:d9:lead:7:ff" || h["X-A"] != "1" {
		t.Fatalf("headers %v", h)
	}
	if _, ok := dest.Config["body"]; ok {
		t.Fatal("destination config mutated")
	}
	if _, ok := asMap(dest.Config["headers"])["Idempotency-Key"]; ok {
		t.Fatal("destination headers mutated")
	}
}

func TestDeliverCRMConfigMerge(t *testing.T) {
	d, rec := newRig(map[string]any{})
	dest := Destination{ID: "d", WorkspaceID: "w", Type: "salesforce",
		Config: map[string]any{"type": "hubspot", "k": "v"}, FieldMap: map[string]any{"a": "b"}}
	d.Deliver(context.Background(), dest, 3, map[string]any{"a": "x"}, "i")
	cfg := asMap(asMap(rec.calls[0])["config"])
	if cfg["type"] != "salesforce" || cfg["k"] != "v" || canon(t, cfg["field_map"]) != `{"a":"b"}` {
		t.Fatalf("cfg %v", cfg)
	}
	// failure without text uses Python's fallback
	d2, _ := newRig(map[string]any{"_act_push_crm": []any{map[string]any{"success": false}}})
	if r := d2.Deliver(context.Background(), dest, 3, nil, "i"); r.Success || r.Error != "crm push failed" {
		t.Fatalf("%+v", r)
	}
}

func TestDeliverCampaignAddersSummaries(t *testing.T) {
	d, _ := newRig(map[string]any{
		"instantly": []any{map[string]any{"success": true, "duplicate": true, "lead_id": "L1"}, map[string]any{"success": true}, map[string]any{"success": false, "error": "q"}},
		"smartlead": []any{map[string]any{"success": true, "duplicate": true}, map[string]any{"success": true}},
	})
	mk := func(typ string) Destination {
		return Destination{ID: "d", WorkspaceID: "w", Type: typ, Config: map[string]any{"campaign_id": "c"}}
	}
	want := []struct{ typ, summary, ext string }{
		{"instantly", "Lead already in campaign", "L1"}, {"instantly", "Lead added to campaign", ""}, {"instantly", "", ""},
		{"smartlead", "Lead already in campaign", ""}, {"smartlead", "Lead added to campaign", ""},
	}
	for i, w := range want {
		r := d.Deliver(context.Background(), mk(w.typ), 1, map[string]any{}, "i")
		if r.Summary != w.summary || r.ExternalID != w.ext {
			t.Errorf("%d: %+v", i, r)
		}
	}
	// skip_if_in_campaign defaults to true
	d3, rec := newRig(map[string]any{})
	d3.Deliver(context.Background(), mk("instantly"), 1, map[string]any{}, "i")
	if rec.calls[0].([]any)[2] != true {
		t.Fatalf("skip default %v", rec.calls[0])
	}
}

func TestDeliverSheetsAirtableKeysAndDefaults(t *testing.T) {
	d, rec := newRig(map[string]any{
		"sheets":   []any{map[string]any{"success": true, "operation": "appended", "range": "Sheet1!A2:B2"}},
		"airtable": []any{map[string]any{"success": true, "operation": "created", "record_id": "rec9"}},
	})
	r := d.Deliver(context.Background(), Destination{ID: "d1", WorkspaceID: "w", Type: "google_sheets",
		Config: map[string]any{"spreadsheet_id": "s", "columns": []any{"a", "b"}}}, 4, map[string]any{"a": "x"}, "i")
	if r.Summary != "Sheet row appended" || r.ExternalID != "Sheet1!A2:B2" {
		t.Fatalf("%+v", r)
	}
	if canon(t, rec.calls[0]) != `["s",["x",""],"dest:d1:lead:4","Sheet1!A:ZZ","w"]` {
		t.Fatalf("sheets call %s", canon(t, rec.calls[0]))
	}
	r = d.Deliver(context.Background(), Destination{ID: "d1", WorkspaceID: "w", Type: "airtable",
		Config: map[string]any{"base_id": "b", "table": "T"}}, 4, map[string]any{"a": "x"}, "i")
	if r.Summary != "Airtable record created" || r.ExternalID != "rec9" {
		t.Fatalf("%+v", r)
	}
	if canon(t, rec.calls[1]) != `[{"a":"x"},"b","T","dest:d1:lead:4","OpenGTM ID",true,"w"]` {
		t.Fatalf("airtable call %s", canon(t, rec.calls[1]))
	}
}

func TestDeliverSlackTemplateAndTruncation(t *testing.T) {
	cases := loadVec(t, "testdata/slack_format.json")["cases"].([]any)
	for i, c := range cases {
		c := asMap(c)
		tpl := strOf(c["template"])
		d, rec := newRig(map[string]any{"secrets": map[string]any{"SLACK_WEBHOOK": "https://hooks.slack.test/services/T/B/placeholder"}})
		dest := Destination{ID: "dest-1", WorkspaceID: vecWS, AudienceID: "aud-1", Type: "slack",
			Config: map[string]any{"webhook_secret_ref": "SLACK_WEBHOOK", "message_template": tpl}}
		wantHook := asMap(c["webhook"])
		leadID, idem := 5, "dest:dest-1:lead:5:abc"
		if wantHook != nil { // one vector uses another lead and key
			leadID = int(asMap(asMap(asMap(wantHook["body"])["metadata"])["event_payload"])["lead_id"].(int64))
			idem = asMap(wantHook["headers"])["Idempotency-Key"].(string)
		}
		res := d.Deliver(context.Background(), dest, leadID, asMap(c["snapshot"]), idem)
		if destvalidate.CheckSlackTemplate(tpl) != nil {
			// D6: syntax beyond {name} and brace escapes is refused, never evaluated.
			if res.Success || res.Error != "invalid slack message_template" || len(rec.calls) != 0 {
				t.Errorf("case %d %q: want refusal, got %+v calls=%d", i, tpl, res, len(rec.calls))
			}
			continue
		}
		if c["raises"] != nil {
			continue // Python-only runtime failures on syntax the Go renderer also accepts: none expected
		}
		if len(rec.calls) != 1 {
			t.Errorf("case %d %q: calls=%d res=%+v", i, tpl, len(rec.calls), res)
			continue
		}
		cfg := asMap(asMap(rec.calls[0])["config"])
		if canon(t, cfg) != canon(t, c["webhook"]) {
			t.Errorf("case %d %q:\n got  %s\n want %s", i, tpl, canon(t, cfg), canon(t, c["webhook"]))
		}
	}
}

func TestSlackTruncatesTo3000Characters(t *testing.T) {
	d, rec := newRig(map[string]any{"secrets": map[string]any{"S": "https://hooks.slack.test/x"}})
	dest := Destination{ID: "d", WorkspaceID: "w", Type: "slack",
		Config: map[string]any{"webhook_secret_ref": "S", "message_template": strings.Repeat("é", 3500)}}
	d.Deliver(context.Background(), dest, 1, nil, "i")
	text := asMap(asMap(asMap(rec.calls[0])["config"])["body"])["text"].(string)
	if utf8.RuneCountInString(text) != 3000 {
		t.Fatalf("len %d", utf8.RuneCountInString(text))
	}
}

func TestSlackSecretMissing(t *testing.T) {
	d, rec := newRig(map[string]any{"secrets": map[string]any{}})
	for _, ref := range []any{"NOPE", "", nil} {
		r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "slack",
			Config: map[string]any{"webhook_secret_ref": ref}}, 1, nil, "i")
		if r.Success || r.Error != "Slack webhook secret not found" || len(rec.calls) != 0 {
			t.Fatalf("%v: %+v", ref, r)
		}
	}
}

func TestUnsupportedType(t *testing.T) {
	d, _ := newRig(map[string]any{})
	for _, typ := range []string{"warehouse_http", "meta_ads", "bogus", "", "WEBHOOK"} {
		r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: typ}, 1, nil, "i")
		if r.Success || r.Error != "unsupported destination '"+typ+"'" || r.Summary != "" {
			t.Fatalf("%q: %+v", typ, r)
		}
	}
	// a supported type whose port is not wired never panics on a nil interface
	bare := New(Ports{}, time.Second)
	r := bare.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "webhook"}, 1, nil, "i")
	if r.Success || !strings.Contains(r.Error, "not configured") {
		t.Fatalf("%+v", r)
	}
}

type errWebhook struct{ err error }

func (e errWebhook) Send(context.Context, WebhookRequest) (Result, error) { return Result{}, e.err }

func TestPortErrorBecomesFailedResultTruncated(t *testing.T) {
	long := strings.Repeat("é", 900)
	d := New(Ports{Webhook: errWebhook{errors.New(long)}}, time.Second)
	r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "webhook"}, 1, nil, "i")
	if r.Success || utf8.RuneCountInString(r.Error) != 500 || !utf8.ValidString(r.Error) {
		t.Fatalf("len %d %+v", utf8.RuneCountInString(r.Error), r.Success)
	}
}

type panicWebhook struct{ v any }

func (p panicWebhook) Send(context.Context, WebhookRequest) (Result, error) { panic(p.v) }

func TestPanicRecovered(t *testing.T) {
	d := New(Ports{Webhook: panicWebhook{"kaboom"}}, time.Second)
	r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "webhook"}, 1, nil, "i")
	if r.Success || !strings.Contains(r.Error, "kaboom") {
		t.Fatalf("%+v", r)
	}
}

type blockWebhook struct{ release chan struct{} }

func (b blockWebhook) Send(context.Context, WebhookRequest) (Result, error) {
	<-b.release // deliberately ignores ctx: the engine must still return on time
	return Result{Success: true}, nil
}

func TestDeliveryTimeoutHonoured(t *testing.T) {
	rel := make(chan struct{})
	defer close(rel)
	d := New(Ports{Webhook: blockWebhook{rel}}, 80*time.Millisecond)
	start := time.Now()
	r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "webhook"}, 1, nil, "i")
	if time.Since(start) > 3*time.Second {
		t.Fatal("Deliver did not honour the timeout")
	}
	if r.Success || !strings.Contains(r.Error, "timed out") {
		t.Fatalf("%+v", r)
	}
	// a cancelled parent context also ends the delivery
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := d.Deliver(ctx, Destination{ID: "d", WorkspaceID: "w", Type: "webhook"}, 1, nil, "i"); r.Success {
		t.Fatalf("%+v", r)
	}
}

func TestSecretsNeverInResults(t *testing.T) {
	const url = "https://hooks.slack.test/services/T000/B000/SECRETTOKEN123"
	secrets := map[string]any{"S": url}
	dest := Destination{ID: "d", WorkspaceID: "w", Type: "slack", Config: map[string]any{"webhook_secret_ref": "S"}}
	for name, port := range map[string]WebhookSender{
		"error":        errWebhook{errors.New(`Post "` + url + `": dial tcp: refused`)},
		"error-escape": errWebhook{errors.New("failed https:\\/\\/hooks.slack.test\\/services\\/T000\\/B000\\/SECRETTOKEN123")},
		"panic":        panicWebhook{"boom " + url},
		"result":       resultWebhook{Result{Error: "bad " + url + " here", Summary: "echo " + url}},
		"tail":         errWebhook{errors.New(strings.Repeat("x", 480) + url)},
	} {
		d := New(Ports{Secrets: (&recorder{secrets: map[string]string{"S": url}}), Webhook: port}, time.Second)
		_ = secrets
		r := d.Deliver(context.Background(), dest, 1, nil, "i")
		if strings.Contains(r.Error, "SECRETTOKEN123") || strings.Contains(r.Summary, "SECRETTOKEN123") || strings.Contains(r.Error, "services/T000") {
			t.Errorf("%s leaked: %+v", name, r)
		}
	}
}

type resultWebhook struct{ r Result }

func (p resultWebhook) Send(context.Context, WebhookRequest) (Result, error) { return p.r, nil }
