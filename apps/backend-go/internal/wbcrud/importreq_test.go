package wbcrud

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func impJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Expected entries are pydantic's own output for ImportRowsRequest (body
// loc prefix added by FastAPI).
func TestReadImportRequestErrors(t *testing.T) {
	long := strings.Repeat("x", 256)
	cases := []struct {
		name, body, want string
	}{
		{"empty body", ``, `[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]`},
		{"missing rows", `{}`, `[{"type":"missing","loc":["body","rows"],"msg":"Field required","input":{}}]`},
		{"empty rows", `{"rows":[]}`, `[{"type":"too_short","loc":["body","rows"],"msg":"List should have at least 1 item after validation, not 0","input":[],"ctx":{"actual_length":0,"field_type":"List","min_length":1}}]`},
		{"row not dict", `{"rows":[1,{"a":1}]}`, `[{"type":"dict_type","loc":["body","rows",0],"msg":"Input should be a valid dictionary","input":1}]`},
		{"rows not list", `{"rows":"x"}`, `[{"type":"list_type","loc":["body","rows"],"msg":"Input should be a valid list","input":"x"}]`},
		{"mapping value", `{"rows":[{"a":1}],"mapping":{"a":5,"b":null,"c":"x"}}`, `[{"type":"string_type","loc":["body","mapping","a"],"msg":"Input should be a valid string","input":5}]`},
		{"mapping list", `{"rows":[{"a":1}],"mapping":[]}`, `[{"type":"dict_type","loc":["body","mapping"],"msg":"Input should be a valid dictionary","input":[]}]`},
		{"bad bool", `{"rows":[{"a":1}],"create_columns":"maybe","dedupe":"no"}`, `[{"type":"bool_parsing","loc":["body","create_columns"],"msg":"Input should be a valid boolean, unable to interpret input","input":"maybe"}]`},
		{"file name long", `{"rows":[{"a":1}],"file_name":"` + long + `"}`, `[{"type":"string_too_long","loc":["body","file_name"],"msg":"String should have at most 255 characters","input":"` + long + `","ctx":{"max_length":255}}]`},
		{"file name int", `{"rows":[{"a":1}],"file_name":5}`, `[{"type":"string_type","loc":["body","file_name"],"msg":"Input should be a valid string","input":5}]`},
		{"source system", `{"rows":[{"a":1}],"source_system":"hubspot"}`, `[{"type":"literal_error","loc":["body","source_system"],"msg":"Input should be 'auto', 'clay' or 'generic'","input":"hubspot","ctx":{"expected":"'auto', 'clay' or 'generic'"}}]`},
		{"source system null", `{"rows":[{"a":1}],"source_system":null}`, `[{"type":"literal_error","loc":["body","source_system"],"msg":"Input should be 'auto', 'clay' or 'generic'","input":null,"ctx":{"expected":"'auto', 'clay' or 'generic'"}}]`},
		{"body not object", `[1]`, `[{"type":"model_attributes_type","loc":["body"],"msg":"Input should be a valid dictionary or object to extract fields from","input":[1]}]`},
		{"invalid json", `{"rows":`, `[{"type":"json_invalid","loc":["body",0],"msg":"JSON decode error","input":{}}]`},
	}
	for _, c := range cases {
		_, errs, err := ReadImportRequest(strings.NewReader(c.body), 1<<20)
		if err != nil {
			t.Errorf("%s: read error %v", c.name, err)
			continue
		}
		var got, want any
		_ = json.Unmarshal([]byte(impJSON(t, errs)), &got)
		if e := json.Unmarshal([]byte(c.want), &want); e != nil {
			t.Fatalf("%s: bad want: %v", c.name, e)
		}
		if impJSON(t, got) != impJSON(t, want) {
			t.Errorf("%s:\n got %s\nwant %s", c.name, impJSON(t, got), impJSON(t, want))
		}
	}
}

func TestReadImportRequestDefaultsAndRows(t *testing.T) {
	req, errs, err := ReadImportRequest(strings.NewReader(`{"rows":[{"B":1.50,"a":"é"},{}],"mapping":{"a":null,"B":"x"},"file_name":"f.csv"}`), 1<<20)
	if err != nil || errs != nil {
		t.Fatalf("err=%v errs=%v", err, errs)
	}
	if !req.CreateColumns || !req.Dedupe || req.SourceSystem != "auto" || req.FileName == nil || *req.FileName != "f.csv" {
		t.Errorf("defaults wrong: %+v", req)
	}
	if len(req.Rows) != 2 || impJSON(t, req.Rows[0]) != `{"B":1.5,"a":"é"}` {
		t.Errorf("rows decoded wrong: %s", impJSON(t, req.Rows))
	}
	if m := req.Mapping; len(m) != 2 || m["a"] != nil || m["B"] == nil || *m["B"] != "x" {
		t.Errorf("mapping decoded wrong: %v", m)
	}
}

func TestImport50001Rows422(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for i := 0; i < MaxImportRows+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"a":1}`)
	}
	b.WriteString(`]}`)
	_, errs, err := ReadImportRequest(strings.NewReader(b.String()), 1<<30)
	if err != nil || len(errs) != 1 {
		t.Fatalf("err=%v errs=%v", err, errs)
	}
	e := errs[0]
	if e.Type != "too_long" || e.Msg != "List should have at most 50000 items after validation, not 50001" ||
		e.Ctx["max_length"] != 50000 || e.Ctx["actual_length"] != 50001 || e.Ctx["field_type"] != "List" {
		t.Errorf("wrong error: %+v", e)
	}
	// exactly 50000 is accepted
	ok := strings.Replace(b.String(), `{"a":1},{"a":1}]}`, `{"a":1}]}`, 1)
	if req, errs, err := ReadImportRequest(strings.NewReader(ok), 1<<30); err != nil || errs != nil || len(req.Rows) != MaxImportRows {
		t.Errorf("50000 rows: err=%v errs=%v rows=%d", err, errs, len(req.Rows))
	}
}

// endless yields an unbounded JSON body and counts what was consumed.
type endless struct {
	n    int64
	head bool
}

func (e *endless) Read(p []byte) (int, error) {
	if !e.head {
		e.head = true
		e.n += int64(len(`{"rows":[`))
		return copy(p, `{"rows":[`), nil
	}
	const chunk = `{"a":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},`
	for i := range p {
		p[i] = chunk[(e.n-9+int64(i))%int64(len(chunk))]
	}
	e.n += int64(len(p))
	return len(p), nil
}

func TestReadImportRequestNeverReadsUnboundedBody(t *testing.T) {
	src := &endless{}
	const limit = 1 << 20
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, _, err := ReadImportRequest(src, limit)
		done <- result{err}
	}()
	var err error
	select {
	case r := <-done:
		err = r.err
	case <-time.After(10 * time.Second):
		t.Fatal("ReadImportRequest did not stop on an endless body")
	}
	if !errors.Is(err, ErrImportBodyTooLarge) {
		t.Fatalf("want ErrImportBodyTooLarge, got %v", err)
	}
	if src.n > limit+(256<<10) {
		t.Errorf("read %d bytes of an endless body with a %d byte limit", src.n, limit)
	}
	// a body exactly at the limit is fine, one byte over is not
	body := `{"rows":[{"a":1}]}`
	if _, errs, err := ReadImportRequest(strings.NewReader(body), int64(len(body))); err != nil || errs != nil {
		t.Errorf("at limit: %v %v", err, errs)
	}
	if _, _, err := ReadImportRequest(io.MultiReader(strings.NewReader(body), strings.NewReader(" ")), int64(len(body))); !errors.Is(err, ErrImportBodyTooLarge) {
		t.Errorf("over limit: %v", err)
	}
}

func TestDeepNestingIsRejectedNotFatal(t *testing.T) {
	deep := strings.Repeat("[", 1<<20)
	bodies := []string{
		deep,
		`{"rows":` + deep,
		`{"rows":[{"a":` + deep,
		`{"mapping":` + deep,
		`{"rows":[{"a":1}],"file_name":` + strings.Repeat(`{"k":`, 1<<18),
	}
	for i, b := range bodies {
		_, errs, err := ReadImportRequest(strings.NewReader(b), 64<<20)
		if err != nil || len(errs) != 1 || errs[0].Type != "json_invalid" {
			t.Errorf("import body %d: err=%v errs=%v", i, err, errs)
		}
	}
	// the shared decoder protects every other route that uses it
	for _, b := range []string{deep, `{"rows":` + deep, `{"updates":[{"fields":` + strings.Repeat(`{"k":`, 1<<18)} {
		if _, errs := DecodeBody([]byte(b)); len(errs) != 1 || errs[0].Type != "json_invalid" {
			t.Errorf("DecodeBody: %v", errs)
		}
	}
	// 100 levels is still fine
	ok := `{"rows":[{"a":` + strings.Repeat("[", 100) + strings.Repeat("]", 100) + `}]}`
	if _, errs, err := ReadImportRequest(strings.NewReader(ok), 1<<20); err != nil || errs != nil {
		t.Errorf("100 levels refused: %v %v", err, errs)
	}
}
