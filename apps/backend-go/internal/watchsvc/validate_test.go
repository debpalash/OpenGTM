package watchsvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

func pv(t *testing.T, raw string) any {
	t.Helper()
	v, err := autosvc.ParseJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type vecErr struct {
	Status  int    `json:"status"`
	Detail  string `json:"detail"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

func checkErr(t *testing.T, got error, want *vecErr) {
	t.Helper()
	var he *HTTPError
	if want.Type != "" { // Python crashed; Go answers 422 (D4)
		if !errors.As(got, &he) || he.Status != 422 {
			t.Fatalf("python crash %s (%s): want 422, got %v", want.Type, want.Message, got)
		}
		return
	}
	if !errors.As(got, &he) {
		t.Fatalf("want %d %q, got %v", want.Status, want.Detail, got)
	}
	if he.Status != want.Status || he.Detail != want.Detail {
		t.Fatalf("got %d %q, want %d %q", he.Status, he.Detail, want.Status, want.Detail)
	}
}

func loadVectors(t *testing.T) (jc []struct {
	MaxContacts int             `json:"max_contacts"`
	Config      json.RawMessage `json:"config"`
	Result      json.RawMessage `json:"result"`
	Error       *vecErr         `json:"error"`
}) {
	raw, err := os.ReadFile("testdata/validate_watch_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		JC []struct {
			MaxContacts int             `json:"max_contacts"`
			Config      json.RawMessage `json:"config"`
			Result      json.RawMessage `json:"result"`
			Error       *vecErr         `json:"error"`
		} `json:"job_change_config"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.JC
}

// expandRepeat turns the vector file's {"repeat": X, "count": N} placeholder
// into a list of N copies of X.
func expandRepeat(t *testing.T, raw json.RawMessage) json.RawMessage {
	var m struct {
		Contacts struct {
			Repeat json.RawMessage `json:"repeat"`
			Count  int             `json:"count"`
		} `json:"contacts"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Contacts.Count == 0 || m.Contacts.Repeat == nil {
		return raw
	}
	var generic map[string]json.RawMessage
	_ = json.Unmarshal(raw, &generic)
	items := bytes.Repeat(append(append([]byte{}, m.Contacts.Repeat...), ','), m.Contacts.Count)
	generic["contacts"] = json.RawMessage("[" + string(items[:len(items)-1]) + "]")
	b, _ := json.Marshal(generic)
	return b
}

func TestWatchValidateVectorsJobChangeConfig(t *testing.T) {
	for i, v := range loadVectors(t) {
		cfgRaw := expandRepeat(t, v.Config)
		var cfg any
		if string(cfgRaw) != "null" {
			cfg = pv(t, string(cfgRaw))
		}
		got, err := ValidateJobChangeConfig(cfg, v.MaxContacts)
		if v.Error != nil {
			checkErr(t, err, v.Error)
			continue
		}
		if err != nil {
			t.Fatalf("vector %d: %v", i, err)
		}
		var want any
		want = pv(t, string(v.Result))
		if string(marshal(got)) != string(marshal(want)) {
			t.Fatalf("vector %d (%s):\n got %s\nwant %s", i, cfgRaw, marshal(got), marshal(want))
		}
	}
}

func TestCursorRoundTripAndBadInputs(t *testing.T) {
	raw, err := os.ReadFile("testdata/cursor_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Vectors []struct {
			Op        string     `json:"op"`
			CreatedAt *string    `json:"created_at"`
			ID        string     `json:"id"`
			Encoded   string     `json:"encoded"`
			Cursor    string     `json:"cursor"`
			Decoded   *[2]string `json:"decoded"`
			Error     *vecErr    `json:"error"`
			_         json.RawMessage
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for i, v := range f.Vectors {
		switch v.Op {
		case "roundtrip", "roundtrip_naive":
			ca := ""
			if v.CreatedAt != nil {
				ca = *v.CreatedAt
			}
			if got := EncodeCursor(ca, v.ID); got != v.Encoded {
				t.Fatalf("vector %d encode: %q want %q", i, got, v.Encoded)
			}
			fallthrough
		case "decode":
			c := v.Cursor
			if v.Op != "decode" {
				c = v.Encoded
			}
			ts, id, err := DecodeCursor(c)
			if v.Error != nil {
				checkErr(t, err, v.Error)
				continue
			}
			if err != nil {
				t.Fatalf("vector %d decode(%q): %v", i, c, err)
			}
			want, perr := time.Parse(time.RFC3339Nano, v.Decoded[0])
			if perr != nil {
				t.Fatal(perr)
			}
			if !ts.Equal(want) || id != v.Decoded[1] {
				t.Fatalf("vector %d: got %v %q want %v %q", i, ts, id, want, v.Decoded[1])
			}
		default:
			t.Fatalf("unknown op %q", v.Op)
		}
	}
}

func TestParseISOForms(t *testing.T) {
	for in, want := range map[string]string{
		"2026-08-29":                       "2026-08-29T00:00:00Z",
		"20260829":                         "2026-08-29T00:00:00Z",
		"2026-08-29T09:00:00Z":             "2026-08-29T09:00:00Z",
		"2026-08-29 09:00":                 "2026-08-29T09:00:00Z",
		"2026-08-29T09:00:00.5+05:30":      "2026-08-29T03:30:00.5Z",
		"2026-08-29T09:00:00.1234567-0100": "2026-08-29T10:00:00.123456Z",
		"20260829T090000Z":                 "2026-08-29T09:00:00Z",
		"2026-08-29T09":                    "2026-08-29T09:00:00Z",
	} {
		got, ok := parseISO(in)
		if !ok {
			t.Errorf("%q rejected", in)
			continue
		}
		w, _ := time.Parse(time.RFC3339Nano, want)
		if !got.Equal(w) {
			t.Errorf("%q = %v want %v", in, got, w)
		}
	}
	for _, bad := range []string{"", "x", "2026-13-01", "2026-02-30", "2026-08-29T", "2026-08-29T25:00", "2026-08-29T09:00:00+25:00", "2026-08-29T09:0", "26-08-29"} {
		if _, ok := parseISO(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestWatchJSONKeyOrderAccountGroupExtras(t *testing.T) {
	ts := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	iv := "daily"
	le := "http_500"
	w := Watch{ID: "w1", WorkspaceID: "ws", Kind: "account_group", Target: "raw target", LeadID: nil,
		SignalTypes: pv(t, `["funding"]`), Interval: &iv, Enabled: true, NextPollAt: &ts, LastError: &le, ConsecutiveFailures: 2,
		Config: pv(t, `{"workbook_name":"Q3 accounts","workbook_id":"wb1","scope_key":"sk","accounts":[{"account_id":"a1","lead_id":4,"company":"Acme"},{"lead_id":5},"junk",{"account_id":"a2"}]}`),
		Cursor: pv(t, `{"attempt_count": "3", "collector_health": {"rss": "ok"}}`), CreatedAt: &ts}
	want := `{"id":"w1","workspace_id":"ws","kind":"account_group","target":"Q3 accounts","resolved_cik":null,"lead_id":null,` +
		`"signal_types":["funding"],"interval":"daily","config":{"workbook_name":"Q3 accounts","workbook_id":"wb1","scope_key":"sk",` +
		`"accounts":[{"account_id":"a1","lead_id":4,"company":"Acme"},{"lead_id":5},"junk",{"account_id":"a2"}]},"enabled":true,` +
		`"next_poll_at":"2026-08-29T09:00:00+00:00","last_polled_at":null,"last_error":"http_500","consecutive_failures":2,` +
		`"cursor":{"attempt_count":"3","collector_health":{"rss":"ok"}},"created_at":"2026-08-29T09:00:00+00:00",` +
		`"workbook_id":"wb1","scope_key":"sk","account_count":4,"account_ids":["a1","a2"],"state":"degraded","attempt_count":3,` +
		`"last_error_class":"http_500","next_retry_at":"2026-08-29T09:00:00+00:00","collector_health":{"rss":"ok"},` +
		`"manual_retry_action":{"method":"POST","url":"/api/watches/w1/poll","label":"Retry collectors now"}}`
	if got := string(marshal(w)); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// plain kinds carry no extras; paused account groups say so
	plain := Watch{ID: "w2", WorkspaceID: "ws", Kind: "funding", Target: "Acme", Enabled: true}
	if got := string(marshal(plain)); got != `{"id":"w2","workspace_id":"ws","kind":"funding","target":"Acme","resolved_cik":null,"lead_id":null,"signal_types":[],"interval":null,"config":{},"enabled":true,"next_poll_at":null,"last_polled_at":null,"last_error":null,"consecutive_failures":0,"cursor":{},"created_at":null}` {
		t.Fatalf("plain: %s", got)
	}
	w.Enabled = false
	if o, _ := wbcrud.ParseObject(marshal(w)); func() any { v, _ := o.Get("state"); return v }() != "paused" {
		t.Fatal("disabled account_group must be paused")
	}
	id := "x"
	rid := Created{Watch: plain, WebhookRuleID: &id}
	if got := string(marshal(rid)); !bytes.HasSuffix([]byte(got), []byte(`"created_at":null,"webhook_rule_id":"x"}`)) {
		t.Fatalf("created: %s", got)
	}
}

func TestRequireEnabledPrecedence(t *testing.T) {
	if e := RequireEnabled(false, false); e == nil || e.Status != 404 || e.Detail != "intent poller disabled" {
		t.Fatalf("%v", e)
	}
	if e := RequireEnabled(true, false); e == nil || e.Status != 409 || e.Detail != "intent_poller_requires_pg_lead_store" {
		t.Fatalf("%v", e)
	}
	if RequireEnabled(true, true) != nil {
		t.Fatal("enabled")
	}
}
