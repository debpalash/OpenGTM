package govaudit

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type vectors struct {
	Encode []struct {
		ISO, ID, Cursor string
	} `json:"encode"`
	Decode []struct {
		Cursor string `json:"cursor"`
		OK     bool   `json:"ok"`
		Time   string `json:"time"`
		ID     string `json:"id"`
		Detail string `json:"detail"`
	} `json:"decode"`
	Time []struct {
		In   string `json:"in"`
		OK   bool   `json:"ok"`
		ISO  string `json:"iso"`
		Type string `json:"type"`
	} `json:"time"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/python_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCursorVectorsMatchPython(t *testing.T) {
	v := loadVectors(t)
	for _, e := range v.Encode {
		parsed, err := parseISO(e.ISO)
		if err != nil {
			t.Fatalf("%s: %v", e.ISO, err)
		}
		if got := EncodeCursor(parsed, e.ID); got != e.Cursor {
			t.Errorf("encode %s: got %s want %s", e.ISO, got, e.Cursor)
		}
	}
	for _, d := range v.Decode {
		tm, id, cerr := DecodeCursor(d.Cursor, "audit event")
		if !d.OK {
			if cerr == nil || cerr.Status() != 422 || cerr.Detail != d.Detail {
				t.Errorf("%q: want 422 %q got %+v", d.Cursor, d.Detail, cerr)
			}
			continue
		}
		if cerr != nil {
			t.Errorf("%q: unexpected %v", d.Cursor, cerr)
			continue
		}
		// Python gives +00:00 for naive; compare instants.
		want, _ := time.Parse(time.RFC3339Nano, d.Time)
		if !tm.Equal(want) || id != d.ID {
			t.Errorf("%q: got %v %q want %v %q", d.Cursor, tm, id, want, d.ID)
		}
	}
}

func TestCursorRejectsGarbageWrongShapeAndNonString(t *testing.T) {
	for _, c := range []string{"!!!", "", "AAAA", "e30", "W10", "WzFd", "WyJ4IiwxXQ", "WyIyMDI2LTAzLTAxVDEyOjAwOjAwIl0", "/w", "bnVsbA", "WzEsMl0"} {
		_, _, cerr := DecodeCursor(c, "retention run")
		if cerr == nil || cerr.Status() != 422 || cerr.Detail != "invalid retention run cursor" {
			t.Errorf("%q: got %+v", c, cerr)
		}
	}
	if _, _, cerr := DecodeCursor("!", "playbook run"); cerr == nil || cerr.Detail != "invalid playbook run cursor" {
		t.Errorf("label: %+v", cerr)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	in := time.Date(2026, 3, 1, 12, 0, 0, 123456000, time.UTC)
	tm, id, cerr := DecodeCursor(EncodeCursor(in, "id-é"), "audit event")
	if cerr != nil || !tm.Equal(in) || id != "id-é" {
		t.Fatalf("%v %v %q", cerr, tm, id)
	}
}

func TestCSVSafeVectors(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "plain": "plain", "=1": "'=1", "+1": "'+1", "-1": "'-1", "@x": "'@x",
		"\tx": "'\tx", "\rx": "'\rx", " =1": "' =1" /* Go hardening, Python leaves it */, "a=": "a=", "'=": "'=",
	} {
		if got := safe(in); got != want {
			t.Errorf("safe(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseTimeParamTable(t *testing.T) {
	for _, r := range loadVectors(t).Time {
		tm, err := ParseTimeParam(r.In)
		if r.OK {
			if err != nil {
				t.Errorf("%q: unexpected %v", r.In, err)
				continue
			}
			if got := isoformat(tm); got != r.ISO {
				t.Errorf("%q: got %s want %s", r.In, got, r.ISO)
			}
			continue
		}
		te, ok := err.(*TimeParamError)
		if !ok || te.Type != r.Type {
			t.Errorf("%q: want error type %s got %v", r.In, r.Type, err)
		}
	}
}

func TestNormalizeMetadataMatchesPython(t *testing.T) {
	in := `{"b":1,"a":{"z":"caf\u00e9","y":[1,2.50,true,null]},"q":"say \"hi\"\nnew"}`
	want := "{\"b\":1,\"a\":{\"z\":\"café\",\"y\":[1,2.5,true,null]},\"q\":\"say \\\"hi\\\"\\nnew\"}"
	got, err := normalizeMetadata([]byte(in))
	if err != nil || string(got) != want {
		t.Fatalf("%v %s", err, got)
	}
	got, _ = normalizeMetadata([]byte(`{"e":1e3,"u":"\u2028<>&\u0001","big":12345678901234567890,"dup":1,"dup":2}`))
	if string(got) != "{\"e\":1000.0,\"u\":\"\u2028<>&\\u0001\",\"big\":12345678901234567890,\"dup\":2}" {
		t.Fatalf("%s", got)
	}
	if got, _ := normalizeMetadata(nil); string(got) != "{}" {
		t.Fatalf("nil -> %s", got)
	}
	if got, _ := normalizeMetadata([]byte("null")); string(got) != "{}" {
		t.Fatalf("null -> %s", got)
	}
}

func TestEventJSONKeyOrder(t *testing.T) {
	e := Event{ID: "i", Method: "GET", Metadata: json.RawMessage(`{"a":1}`), CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	b, err := json.Marshal(e)
	want := `{"id":"i","actor_user_id":null,"actor_role":"","method":"GET","route":"","resource_path":"","response_status":0,"outcome":"","request_id":"","metadata":{"a":1},"created_at":"2026-01-02T03:04:05"}`
	if err != nil || string(b) != want {
		t.Fatalf("%v\n%s", err, b)
	}
}

func TestDecodeCursorLenientLikePythonB64(t *testing.T) {
	raw, err := os.ReadFile("testdata/b64_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs [][2]*string
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		got, ok := pyURLSafeB64Decode(*v[0])
		if v[1] == nil {
			if ok {
				t.Errorf("%q: Python rejects, Go decoded %x", *v[0], got)
			}
			continue
		}
		if !ok || hex.EncodeToString(got) != *v[1] {
			t.Errorf("%q: want %s got %x ok=%v", *v[0], *v[1], got, ok)
		}
	}
	// end to end: junk, '+' and '/', extra padding still decode
	good := EncodeCursor(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), "a")
	for _, c := range []string{good + "!!", good + "==", good[:5] + "." + good[5:], good + "=", " " + good} {
		if _, id, cerr := DecodeCursor(c, "audit event"); cerr != nil || id != "a" {
			t.Errorf("%q: %v", c, cerr)
		}
	}
}

type paramVec struct {
	Query     map[string]string `json:"query"`
	CursorLen int               `json:"cursor_len"`
	Status    int               `json:"status"`
	Body      json.RawMessage   `json:"body"`
}

func TestValidateListParams(t *testing.T) {
	raw, err := os.ReadFile("testdata/params_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []paramVec
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		cursor := v.Query["cursor"]
		if v.CursorLen > 0 {
			cursor = strings.Repeat("a", v.CursorLen)
		}
		limitStr, hasLimit := v.Query["limit"]
		limit, errs := ValidateListParams(cursor, limitStr, hasLimit)
		if v.Status == 200 {
			if len(errs) != 0 {
				t.Errorf("%v: unexpected %+v", v.Query, errs)
			}
			continue
		}
		got, _ := json.Marshal(map[string]any{"detail": errs})
		var a, b any
		_ = json.Unmarshal(got, &a)
		_ = json.Unmarshal(v.Body, &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%v: got %s want %s", v.Query, got, v.Body)
		}
		_ = limit
	}
	if l, errs := ValidateListParams("", "", false); l != 100 || errs != nil {
		t.Fatalf("default %d %v", l, errs)
	}
	for in, want := range map[string]int{" 5 ": 5, "+5": 5, "1_0": 10, "5.0": 5, "05": 5} {
		if l, errs := ValidateListParams("", in, true); l != want || errs != nil {
			t.Errorf("%q: %d %v", in, l, errs)
		}
	}
	if _, errs := ValidateListParams("", "99999999999999999999", true); len(errs) != 1 || errs[0].Type != "less_than_equal" {
		t.Errorf("big: %+v", errs)
	}
}

// Go hardening over Python's safe(): a formula hidden behind leading whitespace, CR or LF is
// neutralised too.
func TestSafeCoversLeadingWhitespaceFormulas(t *testing.T) {
	for in, want := range map[string]string{
		" =1+1": "' =1+1", "\n=cmd": "'\n=cmd", "\r\n@x": "'\r\n@x", "  \t-2": "'  \t-2", " plain": " plain", "a=b": "a=b", "\nplain": "\nplain",
	} {
		if got := safe(in); got != want {
			t.Errorf("safe(%q)=%q want %q", in, got, want)
		}
	}
}
