//go:build integration

package destsync

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Python transcripts of handle_destination_sync for the six plan-07 destination types, recorded with the real
// _deliver and integration clients against a faked network (testdata/adapter_flows.json,
// legacy/python/scripts/gen_dest_adapter_vectors.py). Go runs the same job through destdeliver.NewLivePorts with an
// http.Client whose transport replays the scripted responses, and must send the same requests and write the same rows.

type httpItem struct {
	Status int             `json:"status"`
	JSON   json.RawMessage `json:"json"`
	Text   *string         `json:"text"`
}

type recReq struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	JSON    json.RawMessage   `json:"json"`
}

type adapterScenario struct {
	Name  string `json:"name"`
	Input struct {
		tInput
		HTTP    []httpItem        `json:"http"`
		Secrets map[string]string `json:"secrets"`
	} `json:"input"`
	Output struct {
		tOutput
		Requests []recReq `json:"requests"`
		Unused   int      `json:"unused_http"`
	} `json:"output"`
}

// replay is the fake network: it records each request and answers from the script in order.
type replay struct {
	mu     sync.Mutex
	script []httpItem
	reqs   []recReq
}

func (r *replay) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	rec := recReq{Method: req.Method, URL: req.URL.String(), Headers: map[string]string{}}
	for _, h := range []string{"authorization", "content-type"} {
		if v := req.Header.Get(h); v != "" {
			rec.Headers[h] = v
		}
	}
	if len(body) > 0 {
		rec.JSON = body
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, rec)
	item := httpItem{Status: 599, Text: ptr("unscripted")}
	if len(r.script) > 0 {
		item, r.script = r.script[0], r.script[1:]
	}
	r.mu.Unlock()
	out := []byte{}
	ct := "text/plain; charset=utf-8"
	if len(item.JSON) > 0 {
		var b bytes.Buffer
		_ = json.Compact(&b, item.JSON) // httpx's json= body is compact
		out, ct = b.Bytes(), "application/json"
	} else if item.Text != nil {
		out = []byte(*item.Text)
	}
	return &http.Response{StatusCode: item.Status, Status: http.StatusText(item.Status), Header: http.Header{"Content-Type": {ct}},
		Body: io.NopCloser(bytes.NewReader(out)), ContentLength: int64(len(out)), Request: req, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
}

// memStore is a secrets.Store holding plaintext workspace and global settings.
type memStore struct {
	ws     map[string]map[string]string
	global map[string]string
}

func (m memStore) AppSetting(_ context.Context, key string) (string, error) {
	return m.global[key], nil
}
func (m memStore) WorkspaceSetting(_ context.Context, ws, key string) (string, bool, error) {
	v, ok := m.ws[ws][key]
	return v, ok, nil
}

func loadAdapterScenarios(t *testing.T) []adapterScenario {
	t.Helper()
	raw, err := os.ReadFile("testdata/adapter_flows.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []adapterScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) < 13 {
		t.Fatalf("only %d scenarios", len(f.Scenarios))
	}
	return f.Scenarios
}

// liveDeliverer builds the production ports over the fake network and a resolver with st's settings.
func liveDeliverer(rp *replay, st memStore) *destdeliver.Deliverer {
	res := secrets.NewResolver(nil, st, func(string) string { return "" })
	ports := destdeliver.NewLivePorts(destdeliver.LiveConfig{
		Secrets: res, HTTP: &http.Client{Transport: rp},
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	})
	return destdeliver.New(ports, 30*time.Second)
}

func sameURL(a, b string) bool {
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	if ea != nil || eb != nil {
		return a == b
	}
	return ua.Scheme == ub.Scheme && ua.Host == ub.Host && ua.EscapedPath() == ub.EscapedPath() && reflect.DeepEqual(ua.Query(), ub.Query())
}

// searchFailDeviation rewrites the expected side of a recorded HubSpot/Salesforce scenario in which an email
// search answered non-200 and Python went on to create. Go fails that member at the search (commit 092a609d: no
// duplicate create): the create request and its scripted response are dropped and the delivery error is the
// search failure. Python's behaviour (duplicate create) is not mirrored.
func searchFailDeviation(sc *adapterScenario) {
	typ := sc.Input.Destination.Type
	prefix, isSearch := "", func(r recReq) bool { return false }
	switch typ {
	case "hubspot":
		prefix = "HubSpot contact search failed (retryable): "
		isSearch = func(r recReq) bool { return strings.HasSuffix(r.URL, "/contacts/search") }
	case "salesforce":
		prefix = "Salesforce Lead search failed (retryable): "
		isSearch = func(r recReq) bool { return r.Method == "GET" && strings.Contains(r.URL, "/query") }
	default:
		return
	}
	reqs, script := sc.Output.Requests, sc.Input.HTTP
	var keepReq []recReq
	var keepScript []httpItem
	for i := 0; i < len(reqs); i++ {
		keepReq = append(keepReq, reqs[i])
		if i < len(script) {
			keepScript = append(keepScript, script[i])
		}
		if !isSearch(reqs[i]) || i >= len(script) || script[i].Status == 200 || i+1 >= len(reqs) {
			continue
		}
		body := ""
		if script[i].Text != nil {
			body = *script[i].Text
		} else if len(script[i].JSON) > 0 {
			body = string(script[i].JSON)
		}
		msg := prefix + "HTTP " + strconv.Itoa(script[i].Status) + ": " + body
		// the search request names the lead's email; the delivery of that member carries the error
		for _, m := range sc.Input.Members {
			var snap struct {
				Email string `json:"email"`
			}
			_ = json.Unmarshal(m.Snapshot, &snap)
			if snap.Email == "" || !strings.Contains(string(reqs[i].JSON), `"`+snap.Email+`"`) && !strings.Contains(reqs[i].URL, url.QueryEscape(snap.Email)) {
				continue
			}
			for d := range sc.Output.Deliveries {
				if sc.Output.Deliveries[d].LeadID == m.LeadID {
					sc.Output.Deliveries[d].Error = &msg
				}
			}
		}
		i++ // the create that followed the failed search is never sent
	}
	sc.Output.Requests, sc.Input.HTTP = keepReq, keepScript
}

func TestAdapterSyncMatchesPythonTranscripts(t *testing.T) {
	scs := loadAdapterScenarios(t)
	seen := map[string]bool{}
	for _, sc := range scs {
		sc := sc
		searchFailDeviation(&sc)
		seen[sc.Input.Destination.Type] = true
		t.Run(sc.Name, func(t *testing.T) {
			in := sc.Input
			ws := wsVec
			rp := &replay{script: append([]httpItem(nil), in.HTTP...)}
			st := memStore{ws: map[string]map[string]string{ws: in.Secrets}}
			h := newHarness(t, nil, func(d *Deps) { d.Deliverer = liveDeliverer(rp, st) })
			h.seedDest(ws, destSeed{Type: in.Destination.Type, Config: jstr(in.Destination.Config), FieldMap: jstr(in.Destination.FieldMap),
				Health: in.Destination.Health, Disabled: !in.Destination.Enabled})
			h.seedRun(ws, "run-1", in.RunStatus, "")
			if len(in.Prior) > 0 {
				h.seedRun(ws, "run-0", "completed", "")
			}
			for _, m := range in.Members {
				h.exec(ws, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot) VALUES ($1,$2,$3,$4::json)`,
					ws, audID, m.LeadID, string(m.Snapshot))
			}
			for _, p := range in.Prior {
				var delivered any
				if p.DeliveredAt {
					delivered = "2026-08-01 09:05:00"
				}
				h.exec(ws, `INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint,
					payload, status, attempts, external_id, summary, error, delivered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::json,$9,$10,$11,$12,$13,$14::timestamp)`,
					ws, p.RunID, destID, p.LeadID, p.Operation, p.Key, p.Fingerprint, string(p.Payload), p.Status, p.Attempts, p.ExternalID, p.Summary, p.Error, delivered)
			}
			h.mustHandle(ws, "run-1")
			out := sc.Output

			rr, er := h.run(ws, "run-1"), out.Run
			if rr.Status != er.Status || str(rr.Error) != str(er.Error) || rr.Attempted != er.Attempted || rr.Succeeded != er.Succeeded ||
				rr.Failed != er.Failed || rr.Skipped != er.Skipped || (rr.Finished == nil) != (er.FinishedAt == nil) {
				t.Errorf("run = %+v err=%s\n want %+v err=%s", *rr, str(rr.Error), *er, str(er.Error))
			}
			dr := h.dest(ws)
			if dr.Health != out.Dest.Health || str(dr.LastError) != str(out.Dest.LastError) || (dr.LastSuccess == nil) != (out.Dest.LastSuccess == nil) {
				t.Errorf("destination = %s/%s/%v want %s/%s/%v", dr.Health, str(dr.LastError), dr.LastSuccess, out.Dest.Health, str(out.Dest.LastError), out.Dest.LastSuccess)
			}
			got := h.deliveries(ws)
			if len(got) != len(out.Deliveries) {
				t.Fatalf("%d deliveries, want %d: %+v", len(got), len(out.Deliveries), got)
			}
			byKey := map[string]delRow{}
			for _, d := range got {
				byKey[d.Key] = d
			}
			for _, w := range out.Deliveries {
				g, ok := byKey[w.Key]
				if !ok {
					t.Errorf("missing delivery %s", w.Key)
					continue
				}
				if g.RunID != w.RunID || g.Lead != w.LeadID || g.Status != w.Status || g.Attempts != w.Attempts || g.Fingerprint != w.Fingerprint ||
					str(g.ExternalID) != str(w.ExternalID) || str(g.Summary) != str(w.Summary) || str(g.Error) != str(w.Error) ||
					(g.Delivered == nil) != (w.DeliveredAt == nil) || !jsonEq(json.RawMessage(g.Payload), w.Payload) {
					t.Errorf("delivery lead %d:\n got  status=%s summary=%q error=%q ext=%q\n want status=%s summary=%q error=%q ext=%q",
						w.LeadID, g.Status, str(g.Summary), str(g.Error), str(g.ExternalID), w.Status, str(w.Summary), str(w.Error), str(w.ExternalID))
				}
			}
			if len(rp.reqs) != len(out.Requests) {
				t.Fatalf("%d requests, want %d:\n got  %s\n want %s", len(rp.reqs), len(out.Requests), jstr(rp.reqs), jstr(out.Requests))
			}
			for i, w := range out.Requests {
				g := rp.reqs[i]
				// Deviation (Python bug): sheets.py put spreadsheet_id into the path unquoted, so a "/" or ".." in an
				// editor-supplied id addressed another API path. Go quotes it like every other path segment.
				w.URL = strings.Replace(w.URL, "/spreadsheets/sheet/2/", "/spreadsheets/sheet%2F2/", 1)
				if g.Method != w.Method || !sameURL(g.URL, w.URL) {
					t.Errorf("request %d: %s %s, want %s %s", i, g.Method, g.URL, w.Method, w.URL)
				}
				if g.Headers["authorization"] != w.Headers["authorization"] {
					t.Errorf("request %d authorization %q, want %q", i, g.Headers["authorization"], w.Headers["authorization"])
				}
				if w.JSON != nil && string(w.JSON) != "null" && !jsonEq(g.JSON, w.JSON) {
					t.Errorf("request %d body:\n got  %s\n want %s", i, g.JSON, w.JSON)
				}
				if (w.JSON == nil || string(w.JSON) == "null") && len(g.JSON) > 0 {
					t.Errorf("request %d has a body %s, want none", i, g.JSON)
				}
			}
			if len(rp.script) != out.Unused {
				t.Errorf("%d scripted responses unused, want %d", len(rp.script), out.Unused)
			}
		})
	}
	for _, typ := range []string{"hubspot", "salesforce", "instantly", "smartlead", "google_sheets", "airtable"} {
		if !seen[typ] {
			t.Errorf("no transcript for %s", typ)
		}
	}
}

// TestAdapterCredentialsWorkspaceOnly: Python fell back to the global setting and the environment; Go delivers with
// the destination workspace's own credentials only, so a deployment-level key never reaches an editor-chosen
// campaign, sheet, base or CRM (post-port rule). Every type fails "not connected" and sends nothing.
func TestAdapterCredentialsWorkspaceOnly(t *testing.T) {
	global := map[string]string{}
	for _, sc := range loadAdapterScenarios(t) {
		for k, v := range sc.Input.Secrets {
			global[k] = v
		}
	}
	want := map[string]string{
		"hubspot": "HubSpot not connected", "salesforce": "Salesforce not connected",
		"instantly": "Instantly not connected (set INSTANTLY_API_KEY)", "smartlead": "Smartlead not connected (set SMARTLEAD_API_KEY)",
		"google_sheets": "Google Sheets not connected", "airtable": "Airtable not connected",
	}
	cfgs := map[string]string{
		"hubspot": `{}`, "salesforce": `{}`, "instantly": `{"campaign_id":"c"}`, "smartlead": `{"campaign_id":"1"}`,
		"google_sheets": `{"spreadsheet_id":"s","columns":["email"]}`, "airtable": `{"base_id":"b","table":"t"}`,
	}
	rp := &replay{}
	h := newHarness(t, nil, nil)
	for typ, msg := range want {
		h.again(nil, func(d *Deps) { d.Deliverer = liveDeliverer(rp, memStore{global: global}) })
		h.seedDest(wsVec, destSeed{Type: typ, Config: cfgs[typ]})
		h.seedRun(wsVec, "run-1", "pending", "")
		h.seedMembers(wsVec, snaps(1)...)
		h.mustHandle(wsVec, "run-1")
		if r := h.run(wsVec, "run-1"); r.Status != "completed_with_errors" || r.Failed != 1 {
			t.Errorf("%s: run %+v", typ, *r)
		}
		if d := h.deliveries(wsVec); len(d) != 1 || str(d[0].Error) != msg {
			t.Errorf("%s: deliveries %+v, want error %q", typ, d, msg)
		}
	}
	if len(rp.reqs) != 0 {
		t.Errorf("%d requests sent with global credentials: %s", len(rp.reqs), jstr(rp.reqs))
	}
}
