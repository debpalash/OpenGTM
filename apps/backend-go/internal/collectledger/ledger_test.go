package collectledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type vectors struct {
	Reconcile []struct {
		Status    *string                         `json:"status"`
		WillRetry bool                            `json:"will_retry"`
		Error     string                          `json:"error"`
		Calls     [][]any                         `json:"calls"`
		Payload   map[string]any                  `json:"payload"`
		Raises    *struct{ Type, Message string } `json:"raises"`
	} `json:"reconcile"`
	Isoformat []struct {
		Year, Month, Day, Hour, Minute, Second, Microsecond int
		Tz                                                  string
		Isoformat                                           string
	} `json:"isoformat"`
	JobDetailParse []struct {
		Route        string  `json:"route"`
		Text         *string `json:"text"`
		ResponseBody *string `json:"response_body"`
		RenderError  *string `json:"render_error"`
	} `json:"job_detail_parse"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/ledger_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTimestampFormatIsPythonIsoformat(t *testing.T) {
	zones := map[string]*time.Location{
		"utc":       time.UTC,
		"plus0530":  time.FixedZone("p", 5*3600+30*60),
		"minus0800": time.FixedZone("m", -8*3600),
	}
	for _, c := range loadVectors(t).Isoformat {
		ts := time.Date(c.Year, time.Month(c.Month), c.Day, c.Hour, c.Minute, c.Second, c.Microsecond*1000, zones[c.Tz])
		if got := Isoformat(ts); got != c.Isoformat {
			t.Errorf("%v: got %q want %q", ts, got, c.Isoformat)
		}
	}
	// sub-microsecond precision is truncated like datetime (microsecond resolution).
	if got := Isoformat(time.Date(2026, 1, 2, 3, 4, 5, 999_999_999, time.UTC)); got != "2026-01-02T03:04:05.999999+00:00" {
		t.Errorf("truncation: %q", got)
	}
	if got := Isoformat(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); strings.Contains(got, ".") {
		t.Errorf("zero microseconds must omit the fraction: %q", got)
	}
}

// decodeUseNumber decodes JSON keeping number text (so 1.0 and 1 differ) for DeepEqual.
func decodeUseNumber(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return v
}

func TestInvalidDetailsJSONBecomesEmptyObject(t *testing.T) {
	n := 0
	for _, c := range loadVectors(t).JobDetailParse {
		if c.Route != "stages" || c.Text == nil {
			continue
		}
		n++
		got, err := ParseDetails(*c.Text)
		if c.RenderError != nil {
			// Python 500s here; Go renders NaN, +-Infinity and overflowing floats as null (audit F8).
			want := map[string]string{`NaN`: `null`, `Infinity`: `null`, `{"a":NaN}`: `{"a":null}`,
				`{"big":12345678901234567890,"f":1.0,"e":1e400}`: `{"big":12345678901234567890,"f":1.0,"e":null}`}[*c.Text]
			if err != nil || string(got) != want {
				t.Errorf("%q: want %s, got %q %v", *c.Text, want, got, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", *c.Text, err)
			continue
		}
		var body []struct{ Details json.RawMessage }
		if e := json.Unmarshal([]byte(*c.ResponseBody), &body); e != nil || len(body) != 1 {
			t.Fatalf("vector body: %v", e)
		}
		want := decodeUseNumber(t, body[0].Details)
		if !reflect.DeepEqual(decodeUseNumber(t, got), want) {
			t.Errorf("%q: got %s want %s", *c.Text, got, body[0].Details)
		}
	}
	if n < 15 {
		t.Fatalf("only %d vector rows exercised", n)
	}
}

func TestParseDetailsPythonSemantics(t *testing.T) {
	cases := map[string]string{
		`{"b":1,"a":2,"b":3}`:         `{"b":3,"a":2}`, // duplicate: first position, last value
		`{"z":1.0,"y":1e2,"x":-0}`:    `{"z":1.0,"y":100.0,"x":0}`,
		`{"u":"é😀"}`:                  "{\"u\":\"é\U0001F600\"}",
		"{\"c\":\"a\\u0001\\n\\\"\"}": `{"c":"a\u0001\n\""}`,
		`[1, 2 ,{"k" : null}]`:        `[1,2,{"k":null}]`,
		"  {}\n":                      `{}`,
		`12345678901234567890`:        `12345678901234567890`,
		`{"a":1}x`:                    `{}`,
		"\ufeff{}":                    `{}`,
		`{'a':1}`:                     `{}`,
		"{\"a\":\"\x01\"}":            `{}`, // raw control char is invalid in a JSON string
		`{"a":01}`:                    `{}`,
		`[1,]`:                        `{}`,
		``:                            `{}`,
	}
	for in, want := range cases {
		got, err := ParseDetails(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{`NaN`: `null`, `[Infinity]`: `[null]`, `{"a":-Infinity}`: `{"a":null}`, `1e999`: `null`} {
		if got, err := ParseDetails(in); err != nil || string(got) != want {
			t.Errorf("%q: want %s, got %s %v", in, want, got, err)
		}
	}
	if _, err := ParseDetails(`"\ud800"`); !errors.Is(err, ErrNotRenderable) {
		t.Errorf("lone surrogate: want ErrNotRenderable, got %v", err)
	}
}

func TestFailurePayloadValidation(t *testing.T) {
	n := 0
	for _, c := range loadVectors(t).Reconcile {
		if c.Raises == nil {
			continue
		}
		n++
		s := func(k string) string { v, _ := c.Payload[k].(string); return v }
		err := CheckFailurePayload(s("job_id"), s("workspace_id"), s("slug"))
		if err == nil || err.Error() != c.Raises.Message || !errors.Is(err, ErrFailurePayload) {
			t.Errorf("%v: got %v want %q", c.Payload, err, c.Raises.Message)
		}
	}
	if n != 8 {
		t.Fatalf("expected 8 raising vectors, got %d", n)
	}
	if err := CheckFailurePayload("j", "w", "s"); err != nil {
		t.Fatal(err)
	}
}
