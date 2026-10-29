package ssestream

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReformatVectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"b":1,"a":[1,2,{"z":null,"y":true}]}`, `{"b": 1, "a": [1, 2, {"z": null, "y": true}]}`},
		{"{ \"a\" :\n [ ] , \"b\" : { } }", `{"a": [], "b": {}}`},
		{`{"s":"caf\u00e9 — \ud83d\ude00 é 😀 \/ \b\f"}`, `{"s": "caf\u00e9 \u2014 \ud83d\ude00 \u00e9 \ud83d\ude00 / \b\f"}`},
		{`{"a":"x\"\\\n\u0001\u007f"}`, `{"a": "x\"\\\n\u0001\u007f"}`},
		{`[1.0,-0.0,0.1,1e16,1.5E-7,12345678901234567890,-0,0]`, `[1.0, -0.0, 0.1, 1e+16, 1.5e-07, 12345678901234567890, 0, 0]`},
		{`{"a":1,"a":2}`, `{"a": 1, "a": 2}`}, // duplicate keys kept in source order
		{`"x"`, `"x"`},
		{`null`, `null`},
	}
	for _, c := range cases {
		got, err := Reformat([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

func TestReformatRejects(t *testing.T) {
	deep := strings.Repeat("[", 65) + strings.Repeat("]", 65)
	ok := strings.Repeat("[", 64) + strings.Repeat("]", 64)
	if _, err := Reformat([]byte(ok)); err != nil {
		t.Fatalf("depth 64 should pass: %v", err)
	}
	bad := []string{
		`NaN`, `{"a":NaN}`, `[Infinity]`, `[-Infinity]`, `[1e999]`, `[-1e999]`,
		"\"\xff\"", "{\"a\xfe\":1}", `"\ud800"`, `{"a":1`, `{"a":1} x`, ``, `{"a":}`, `[1,]`,
		deep,
	}
	for _, s := range bad {
		if _, err := Reformat([]byte(s)); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	old := MaxFrameBytes
	MaxFrameBytes = 16
	defer func() { MaxFrameBytes = old }()
	if _, err := Reformat([]byte(`{"a":"` + strings.Repeat("x", 20) + `"}`)); err == nil {
		t.Error("size cap not enforced")
	}
}

func TestEventRawBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := New(rec, nil)
	if err := w.EventRaw([]byte(`{"z":1,"a":"é"}`)); err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "data: {\"z\": 1, \"a\": \"\\u00e9\"}\n\n" {
		t.Fatalf("%q", got)
	}
	if err := w.EventRaw([]byte(`NaN`)); err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(rec.Body.String(), "NaN") {
		t.Fatal("bad frame written")
	}
}

// Every data frame in the recorded Python SSE fixtures round-trips byte-exactly, from the
// Python text itself and from a compacted and an indented copy (different whitespace, same order).
func TestReformatRoundTripsRecordedSSE(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/contract/copilot/*.json")
	n := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Response struct {
				Body any `json:"body"`
			} `json:"response"`
		}
		if json.Unmarshal(b, &fx) != nil {
			continue
		}
		body, ok := fx.Response.Body.(string)
		if !ok || !strings.HasPrefix(body, "data: ") && !strings.HasPrefix(body, ": ") {
			continue
		}
		for _, fr := range strings.Split(body, "\n\n") {
			p, ok := strings.CutPrefix(fr, "data: ")
			if !ok || p == "[DONE]" {
				continue
			}
			var c, ind bytes.Buffer
			if json.Compact(&c, []byte(p)) != nil || json.Indent(&ind, []byte(p), "", "  ") != nil {
				t.Fatalf("%s: bad json %q", f, p)
			}
			for name, in := range map[string][]byte{"orig": []byte(p), "compact": c.Bytes(), "indent": ind.Bytes()} {
				got, err := Reformat(in)
				if err != nil {
					t.Fatalf("%s %s: %v", f, name, err)
				}
				if string(got) != p {
					t.Fatalf("%s %s:\n got %s\nwant %s", f, name, got, p)
				}
			}
			n++
		}
	}
	if n < 30 {
		t.Fatalf("only %d recorded frames checked", n)
	}
}

// RPC frames recorded from the Python worker re-emit with their key order intact and are
// idempotent (Reformat of its own output is unchanged).
func TestReformatRecordedWorkerFrames(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/copilot/frames/*.ndjson")
	n := 0
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var rec struct {
				Frames []json.RawMessage `json:"frames"`
			}
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			for _, fr := range rec.Frames {
				got, err := Reformat(fr)
				if err != nil {
					t.Fatalf("%s: %v on %s", f, err, fr)
				}
				again, err := Reformat(got)
				if err != nil || !bytes.Equal(again, got) {
					t.Fatalf("%s: not idempotent: %s vs %s", f, got, again)
				}
				// same keys in the same order as the source
				if a, b := objKeys(t, fr), objKeys(t, got); strings.Join(a, ",") != strings.Join(b, ",") {
					t.Fatalf("%s: key order changed %v -> %v", f, a, b)
				}
				n++
			}
		}
	}
	if n < 100 {
		t.Fatalf("only %d frames", n)
	}
}

// objKeys lists object keys in document order using a tolerant scan via a key-order decoder.
func objKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out []string
	var isObj []bool
	expectKey := []bool{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if n := len(expectKey); n > 0 && isObj[n-1] {
					expectKey[n-1] = true // value consumed by the container; next is a key
				}
				isObj = append(isObj, d == '{')
				expectKey = append(expectKey, d == '{')
			default:
				isObj = isObj[:len(isObj)-1]
				expectKey = expectKey[:len(expectKey)-1]
			}
			continue
		}
		n := len(expectKey)
		if n > 0 && isObj[n-1] {
			if expectKey[n-1] {
				out = append(out, tok.(string))
			}
			expectKey[n-1] = !expectKey[n-1]
		}
	}
}

func TestReformatFrameCaps(t *testing.T) {
	const cap = 8<<20 + 64<<10 // per-line cap of the browser service
	if MaxFrameBytes != cap {
		t.Fatalf("MaxFrameBytes = %d, want %d", MaxFrameBytes, cap)
	}
	big := `{"d":"` + strings.Repeat("x", 8<<20) + `"}`
	if out, err := Reformat([]byte(big)); err != nil || len(out) != len(big)+1 {
		t.Fatalf("8 MiB frame: err=%v len=%d", err, len(out))
	}
	// ensure_ascii expands raw non-ASCII 2 bytes -> 6: output may exceed the input cap.
	exp := `{"d":"` + strings.Repeat("é", 3<<20) + `"}`
	out, err := Reformat([]byte(exp))
	if err != nil {
		t.Fatalf("expanding frame: %v", err)
	}
	if len(out) <= cap {
		t.Fatalf("expected expansion beyond cap, got %d", len(out))
	}
	if _, err := Reformat([]byte(`"` + strings.Repeat("x", cap) + `"`)); err == nil {
		t.Fatal("oversize input accepted")
	}
}
