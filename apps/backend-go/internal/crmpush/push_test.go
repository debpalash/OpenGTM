package crmpush_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// ── vector file ──────────────────────────────────────────────────────────

type vectors struct {
	UpdateFields []struct {
		CRMType  string         `json:"crm_type"`
		LeadData map[string]any `json:"lead_data"`
		CfgName  string         `json:"cfg_name"`
		Cfg      map[string]any `json:"cfg"`
		Value    map[string]any `json:"value"`
	} `json:"update_fields"`
	PushFlow []pushCase `json:"push_flow"`
}

type pushCase struct {
	Name         string         `json:"name"`
	Cfg          map[string]any `json:"cfg"`
	LeadData     map[string]any `json:"lead_data"`
	CRMConnected bool           `json:"crm_connected"`
	UpdateResult map[string]any `json:"update_result"`
	PushResult   map[string]any `json:"push_result"`
	WorkbookID   *string        `json:"workbook_id"`
	LeadID       *int64         `json:"lead_id"`
	StoreReturns bool           `json:"store_returns"`
	Calls        []vectorCall   `json:"calls"`
	Result       map[string]any `json:"result"`
}

type vectorCall struct {
	Fn          string          `json:"fn"`
	Args        json.RawMessage `json:"args"`
	WorkspaceID string          `json:"workspace_id"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/crmpush.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// orderedKeys returns the top-level key order of a JSON object.
func orderedKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// ── fakes ────────────────────────────────────────────────────────────────

type callRec struct {
	Fn    string
	WS    string
	Props crm.FieldMap // update_*: properties/fields; push: the field map
	ID    string
	Obj   string
	Lead  crm.Lead
}

type fakeHS struct {
	mu        sync.Mutex
	connected bool
	update    crm.UpdateResult
	push      crm.PushResult
	calls     []callRec
}

func (f *fakeHS) IsConnected(context.Context, string) bool { return f.connected }
func (f *fakeHS) UpdateContactByID(_ context.Context, ws, id string, p crm.FieldMap) crm.UpdateResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, callRec{Fn: "update_contact_by_id", WS: ws, ID: id, Props: p})
	return f.update
}
func (f *fakeHS) PushLeadAsContact(_ context.Context, ws string, l crm.Lead, fm crm.FieldMap) crm.PushResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, callRec{Fn: "push_lead_as_contact", WS: ws, Lead: l, Props: fm})
	return f.push
}

type fakeSF struct {
	mu        sync.Mutex
	connected bool
	update    crm.SalesforceUpdateResult
	push      crm.SalesforcePushResult
	calls     []callRec
}

func (f *fakeSF) IsConnected(context.Context, string) bool { return f.connected }
func (f *fakeSF) UpdateRecord(_ context.Context, ws, sobject, id string, fields crm.FieldMap) crm.SalesforceUpdateResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, callRec{Fn: "update_record", WS: ws, Obj: sobject, ID: id, Props: fields})
	return f.update
}
func (f *fakeSF) PushLeadAsContact(_ context.Context, ws string, l crm.Lead, fm crm.FieldMap) crm.SalesforcePushResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, callRec{Fn: "push_lead_as_contact", WS: ws, Lead: l, Props: fm})
	return f.push
}

func str(v any) string { s, _ := v.(string); return s }

func hsUpdate(m map[string]any) crm.UpdateResult {
	if m == nil {
		return crm.UpdateResult{}
	}
	return crm.UpdateResult{Success: m["success"] == true, NotFound: m["not_found"] == true,
		Error: str(m["error"]), Action: str(m["action"]), HubspotID: str(m["hubspot_id"])}
}
func hsPush(m map[string]any) crm.PushResult {
	if m == nil {
		return crm.PushResult{}
	}
	r := crm.PushResult{Success: m["success"] == true, Action: str(m["action"]), Error: str(m["error"])}
	if id, ok := m["hubspot_id"]; ok {
		r.HubspotID = id
	}
	return r
}
func sfUpdate(m map[string]any) crm.SalesforceUpdateResult {
	if m == nil {
		return crm.SalesforceUpdateResult{}
	}
	return crm.SalesforceUpdateResult{Success: m["success"] == true, NotFound: m["not_found"] == true,
		Error: str(m["error"]), Action: str(m["action"]), SalesforceID: str(m["salesforce_id"])}
}
func sfPush(m map[string]any) crm.SalesforcePushResult {
	if m == nil {
		return crm.SalesforcePushResult{}
	}
	r := crm.SalesforcePushResult{Success: m["success"] == true, Action: str(m["action"]), Error: str(m["error"])}
	if id, ok := m["salesforce_id"]; ok {
		r.SalesforceID = id
	}
	return r
}

type storeRec struct {
	WS, WB, CRM, Old, New, Obj string
	LeadID                     int64
}

type harness struct {
	hs     *fakeHS
	sf     *fakeSF
	stores []storeRec
	logs   *bytes.Buffer
	deps   crmpush.Deps
}

func newHarness(storeOK bool, storeErr error) *harness {
	h := &harness{hs: &fakeHS{}, sf: &fakeSF{}, logs: &bytes.Buffer{}}
	h.deps = crmpush.Deps{
		HubSpot:    h.hs,
		Salesforce: h.sf,
		Log:        slog.New(slog.NewTextHandler(h.logs, nil)),
		StoreRowID: func(_ context.Context, ws, wb string, leadID int64, crmType, old, nw, obj string) (bool, error) {
			h.stores = append(h.stores, storeRec{ws, wb, crmType, old, nw, obj, leadID})
			return storeOK, storeErr
		},
	}
	return h
}

func marshalResult(t *testing.T, r crmpush.Result) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ── vectors ──────────────────────────────────────────────────────────────

func TestUpdateFieldsVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.UpdateFields) == 0 {
		t.Fatal("no vectors")
	}
	for i, c := range v.UpdateFields {
		got := crmpush.UpdateFields(c.CRMType, c.Cfg, c.LeadData)
		want := map[string]string{}
		for k, x := range c.Value {
			want[k] = x.(string)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("#%d %s/%s: got %v want %v", i, c.CRMType, c.CfgName, got, want)
		}
	}
}

func TestPushVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.PushFlow) < 30 {
		t.Fatalf("only %d vectors", len(v.PushFlow))
	}
	for _, c := range v.PushFlow {
		t.Run(c.Name, func(t *testing.T) {
			h := newHarness(c.StoreReturns, nil)
			h.hs.connected, h.sf.connected = c.CRMConnected, c.CRMConnected
			h.hs.update, h.hs.push = hsUpdate(c.UpdateResult), hsPush(c.PushResult)
			h.sf.update, h.sf.push = sfUpdate(c.UpdateResult), sfPush(c.PushResult)
			wb := ""
			if c.WorkbookID != nil {
				wb = *c.WorkbookID
			}
			lead := crmpush.NoLeadID
			if c.LeadID != nil {
				lead = *c.LeadID
			}
			got := crmpush.Push(context.Background(), h.deps, "ws-1", c.Cfg, c.LeadData, wb, lead)
			wantJSON, _ := json.Marshal(c.Result)
			// key order success, value, error
			if gj := marshalResult(t, got); gj != `{"success":`+boolStr(c.Result["success"])+`,"value":`+jsonStr(c.Result["value"])+`,"error":`+jsonStrOrNull(c.Result["error"])+`}` {
				t.Fatalf("result json %s (vector %s)", gj, wantJSON)
			}
			// calls (store calls are the vector's _store_row_crm_id entries)
			var crmCalls []vectorCall
			var storeCalls []vectorCall
			for _, vc := range c.Calls {
				if vc.Fn == "_store_row_crm_id" {
					storeCalls = append(storeCalls, vc)
				} else {
					crmCalls = append(crmCalls, vc)
				}
			}
			all := append(append([]callRec{}, h.hs.calls...), h.sf.calls...)
			if len(all) != len(crmCalls) {
				t.Fatalf("crm calls %d want %d", len(all), len(crmCalls))
			}
			for i, vc := range crmCalls {
				assertCall(t, all[i], vc)
			}
			if len(h.stores) != len(storeCalls) {
				t.Fatalf("store calls %d want %d", len(h.stores), len(storeCalls))
			}
			for i, vc := range storeCalls {
				var a struct {
					WB, CRM, Old, New, Obj string
					LID                    int64
				}
				var raw map[string]any
				_ = json.Unmarshal(vc.Args, &raw)
				a.WB, a.CRM, a.Old = str(raw["workbook_id"]), str(raw["crm_type"]), str(raw["old_ext_id"])
				a.New, a.Obj = str(raw["new_id"]), str(raw["new_object"])
				a.LID = int64(raw["lead_id"].(float64))
				s := h.stores[i]
				if s.WS != "ws-1" || s.WB != a.WB || s.CRM != a.CRM || s.Old != a.Old || s.New != a.New || s.Obj != a.Obj || s.LeadID != a.LID {
					t.Fatalf("store %+v want %+v", s, a)
				}
			}
		})
	}
}

func boolStr(v any) string {
	if v == true {
		return "true"
	}
	return "false"
}
func jsonStr(v any) string { b, _ := json.Marshal(v); return string(b) }
func jsonStrOrNull(v any) string {
	if v == nil {
		return "null"
	}
	return jsonStr(v)
}

func assertCall(t *testing.T, got callRec, want vectorCall) {
	t.Helper()
	if got.Fn != want.Fn || got.WS != want.WorkspaceID {
		t.Fatalf("call %s/%s want %s/%s", got.Fn, got.WS, want.Fn, want.WorkspaceID)
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(want.Args, &args); err != nil {
		t.Fatal(err)
	}
	switch want.Fn {
	case "update_contact_by_id", "update_record":
		idKey, propKey := "contact_id", "properties"
		if want.Fn == "update_record" {
			idKey, propKey = "record_id", "fields"
			if got.Obj != str(jv(args["sobject"])) {
				t.Fatalf("sobject %q want %s", got.Obj, args["sobject"])
			}
		}
		if got.ID != str(jv(args[idKey])) {
			t.Fatalf("id %q want %s", got.ID, args[idKey])
		}
		wantKeys := orderedKeys(t, args[propKey])
		var wantVals map[string]string
		_ = json.Unmarshal(args[propKey], &wantVals)
		var gotKeys []string
		for _, p := range got.Props {
			gotKeys = append(gotKeys, p.Key)
			if wantVals[p.Key] != p.Value {
				t.Fatalf("prop %s=%q want %q", p.Key, p.Value, wantVals[p.Key])
			}
		}
		if !reflect.DeepEqual(gotKeys, wantKeys) {
			t.Fatalf("prop order %v want %v", gotKeys, wantKeys)
		}
	case "push_lead_as_contact":
		var lead map[string]any
		_ = json.Unmarshal(args["lead"], &lead)
		for k, x := range lead {
			g, _ := got.Lead.Get(k)
			if g != x {
				t.Fatalf("lead.%s=%v want %v", k, g, x)
			}
		}
		if string(args["field_map"]) == "null" {
			if len(got.Props) != 0 {
				t.Fatalf("field map %v want none", got.Props)
			}
		} else {
			wantKeys := orderedKeys(t, args["field_map"])
			var gotKeys []string
			for _, p := range got.Props {
				gotKeys = append(gotKeys, p.Key)
			}
			if !reflect.DeepEqual(gotKeys, wantKeys) {
				t.Fatalf("field map order %v want %v", gotKeys, wantKeys)
			}
		}
	}
}

func jv(raw json.RawMessage) any { var v any; _ = json.Unmarshal(raw, &v); return v }

// ── named behaviours ─────────────────────────────────────────────────────

func TestUnsupportedCRM(t *testing.T) {
	h := newHarness(true, nil)
	h.hs.connected, h.sf.connected = true, true
	r := crmpush.Push(context.Background(), h.deps, "ws", map[string]any{"type": "  Pipedrive "}, nil, "wb", 1)
	// Python lower() does not strip: the message carries the raw lowered text.
	if r.Success || r.Error != "unsupported crm '  pipedrive '" || r.Value != "" {
		t.Fatalf("%+v", r)
	}
	if len(h.hs.calls)+len(h.sf.calls) != 0 {
		t.Fatal("no CRM call expected")
	}
	if got := marshalResult(t, r); got != `{"success":false,"value":"","error":"unsupported crm '  pipedrive '"}` {
		t.Fatal(got)
	}
}

func TestNotConnectedMessage(t *testing.T) {
	h := newHarness(true, nil)
	for typ, want := range map[string]string{"hubspot": "HubSpot not connected", "SALESFORCE": "Salesforce not connected"} {
		r := crmpush.Push(context.Background(), h.deps, "ws", map[string]any{"type": typ}, map[string]any{"email": "a@b.c"}, "wb", 1)
		if r.Success || r.Error != want {
			t.Fatalf("%s: %+v", typ, r)
		}
	}
	if len(h.hs.calls)+len(h.sf.calls) != 0 {
		t.Fatal("no CRM call when not connected")
	}
}

func TestUpdateByExternalIDOnlyMappedFields(t *testing.T) {
	h := newHarness(true, nil)
	h.hs.connected = true
	h.hs.update = crm.UpdateResult{Success: true}
	lead := map[string]any{"company": "Acme", "email": "a@x.com", "city": "Pune", "crm_external_id": " 501 ", "crm_type": "HubSpot"}
	cfg := map[string]any{"field_map": map[string]any{"company": "company", "email": "email"}}
	r := crmpush.Push(context.Background(), h.deps, "ws", cfg, lead, "wb", 1)
	if !r.Success || r.Value != "HubSpot: updated 501" {
		t.Fatalf("%+v", r)
	}
	if len(h.hs.calls) != 1 || h.hs.calls[0].Fn != "update_contact_by_id" || h.hs.calls[0].ID != "501" {
		t.Fatalf("%+v", h.hs.calls)
	}
	got := map[string]string{}
	for _, p := range h.hs.calls[0].Props {
		got[p.Key] = p.Value
	}
	if !reflect.DeepEqual(got, map[string]string{"company": "Acme", "email": "a@x.com"}) {
		t.Fatalf("city must not be sent: %v", got)
	}
}

func TestContactPersonSplit(t *testing.T) {
	lead := map[string]any{"contact_person": "Ada Byron Lovelace", "email": "a@x.com"}
	cfg := map[string]any{"field_map": map[string]any{"contact_person": "firstname", "email": "email"}}
	if got := crmpush.UpdateFields("hubspot", cfg, lead); !reflect.DeepEqual(got, map[string]string{"firstname": "Ada", "lastname": "Byron Lovelace", "email": "a@x.com"}) {
		t.Fatalf("hubspot %v", got)
	}
	if got := crmpush.UpdateFields("salesforce", cfg, lead); !reflect.DeepEqual(got, map[string]string{"FirstName": "Ada", "LastName": "Byron Lovelace", "email": "a@x.com"}) {
		t.Fatalf("salesforce %v", got)
	}
	// single token: plain mapped field
	if got := crmpush.UpdateFields("hubspot", cfg, map[string]any{"contact_person": "Ada"}); !reflect.DeepEqual(got, map[string]string{"firstname": "Ada"}) {
		t.Fatalf("single %v", got)
	}
	// None and "" skipped, 0 kept as "0"
	if got := crmpush.UpdateFields("hubspot", map[string]any{"field_map": map[string]any{"a": "A", "b": "B", "c": "C"}}, map[string]any{"a": nil, "b": "", "c": 0}); !reflect.DeepEqual(got, map[string]string{"C": "0"}) {
		t.Fatalf("skip %v", got)
	}
}

func TestNotFoundFallsThroughToCreate(t *testing.T) {
	h := newHarness(true, nil)
	h.hs.connected = true
	h.hs.update = crm.UpdateResult{NotFound: true, Error: "contact 501 not found in HubSpot"}
	h.hs.push = crm.PushResult{Success: true, Action: "created", HubspotID: "901"}
	lead := map[string]any{"company": "Acme", "email": "a@x.com", "crm_external_id": "501", "crm_type": "hubspot"}
	r := crmpush.Push(context.Background(), h.deps, "ws", nil, lead, "wb-1", 7)
	if !r.Success || r.Value != "HubSpot: created 901" {
		t.Fatalf("%+v", r)
	}
	if len(h.hs.calls) != 2 || h.hs.calls[1].Fn != "push_lead_as_contact" {
		t.Fatalf("%+v", h.hs.calls)
	}
	if !reflect.DeepEqual(h.stores, []storeRec{{"ws", "wb-1", "hubspot", "501", "901", "contact", 7}}) {
		t.Fatalf("%+v", h.stores)
	}
	// salesforce create makes a Lead sObject
	h2 := newHarness(true, nil)
	h2.sf.connected = true
	h2.sf.update = crm.SalesforceUpdateResult{NotFound: true}
	h2.sf.push = crm.SalesforcePushResult{Success: true, Action: "created", SalesforceID: "00Q1"}
	r = crmpush.Push(context.Background(), h2.deps, "ws", map[string]any{"type": "salesforce"},
		map[string]any{"email": "a@x.com", "crm_external_id": "003A", "crm_type": "salesforce", "crm_object": "contact"}, "wb-1", 7)
	if !r.Success || r.Value != "Salesforce: created 00Q1" || h2.stores[0].Obj != "lead" {
		t.Fatalf("%+v %+v", r, h2.stores)
	}
}

func TestStampFailureDoesNotFailPush(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
		err  error
	}{{"error", false, errors.New("boom: db down")}, {"not matched", false, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(tc.ok, tc.err)
			h.hs.connected = true
			h.hs.update = crm.UpdateResult{NotFound: true}
			h.hs.push = crm.PushResult{Success: true, Action: "created", HubspotID: "901"}
			r := crmpush.Push(context.Background(), h.deps, "ws", nil,
				map[string]any{"email": "a@x.com", "crm_external_id": "501", "crm_type": "hubspot"}, "wb-1", 7)
			if !r.Success || r.Value != "HubSpot: created 901" || r.Error != "" {
				t.Fatalf("%+v", r)
			}
			if tc.err != nil && !strings.Contains(h.logs.String(), "db down") {
				t.Fatalf("stamp failure not logged: %q", h.logs.String())
			}
		})
	}
	// a panicking stamp is a failed stamp too (Python catches Exception)
	h := newHarness(true, nil)
	h.deps.StoreRowID = func(context.Context, string, string, int64, string, string, string, string) (bool, error) {
		panic("kaboom")
	}
	h.hs.connected = true
	h.hs.update = crm.UpdateResult{NotFound: true}
	h.hs.push = crm.PushResult{Success: true, Action: "created", HubspotID: "901"}
	if r := crmpush.Push(context.Background(), h.deps, "ws", nil,
		map[string]any{"email": "a@x.com", "crm_external_id": "501", "crm_type": "hubspot"}, "wb-1", 7); !r.Success {
		t.Fatalf("%+v", r)
	}
}

// ── per-workspace credentials ────────────────────────────────────────────

type secretStore struct {
	app map[string]string
	ws  map[string]string
}

func (s *secretStore) AppSetting(_ context.Context, k string) (string, error) { return s.app[k], nil }
func (s *secretStore) WorkspaceSetting(_ context.Context, ws, k string) (string, bool, error) {
	v, ok := s.ws[ws+"/"+k]
	return v, ok, nil
}

func TestTokenOfOtherWorkspaceNeverUsed(t *testing.T) {
	t.Setenv("HUBSPOT_TOKEN", "")
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: "crmpush-test-master-key", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := e.Encrypt(context.Background(), "tok-of-workspace-a")
	if err != nil {
		t.Fatal(err)
	}
	st := &secretStore{app: map[string]string{}, ws: map[string]string{"ws-a/HUBSPOT_TOKEN": ct}}
	var hits atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	hs := &crm.HubSpot{HTTP: srv.Client(), BaseURL: srv.URL, Secrets: secrets.NewResolver(e, st, func(string) string { return "" })}
	d := crmpush.Deps{HubSpot: hs, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	lead := map[string]any{"email": "a@x.com", "company": "Acme"}

	rb := crmpush.Push(context.Background(), d, "ws-b", nil, lead, "wb", 1)
	if rb.Success || rb.Error != "HubSpot not connected" || hits.Load() != 0 {
		t.Fatalf("ws-b: %+v hits=%d", rb, hits.Load())
	}
	ra := crmpush.Push(context.Background(), d, "ws-a", nil, lead, "wb", 1)
	if ra.Success || hits.Load() == 0 || auth.Load() != "Bearer tok-of-workspace-a" {
		t.Fatalf("ws-a: %+v hits=%d auth=%v", ra, hits.Load(), auth.Load())
	}
	if strings.Contains(ra.Error, "tok-of-workspace-a") {
		t.Fatal("token leaked into result")
	}
}
