package sheets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit/kittest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

func values(t *testing.T, tr *kittest.Transcript, name string) []any {
	t.Helper()
	raw, ok := tr.Call.Args[name]
	if !ok {
		return nil
	}
	v, err := providers.ParseJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := v.([]any)
	return l
}

func TestSheetsCellFormattingVectors(t *testing.T) {
	var n int
	for _, tr := range kittest.Load(t, "sheets") {
		if tr.Call.Function != "_cell_table" {
			continue
		}
		var rows []struct {
			Value json.RawMessage `json:"value"`
			Cell  string          `json:"cell"`
		}
		if err := json.Unmarshal(tr.Result, &rows); err != nil {
			t.Fatal(err)
		}
		for i, v := range values(t, tr, "values") {
			n++
			if got := cell(v); got != rows[i].Cell {
				t.Errorf("cell(%#v) = %q want %q", v, got, rows[i].Cell)
			}
		}
	}
	if n != 18 {
		t.Fatalf("vectors ran %d", n)
	}
	// a plain number with a trailing newline matches Python's `$`.
	if cell("-3\n") != "-3\n" || cell("-3\n\n") != "'-3\n\n" {
		t.Fatalf("trailing newline: %q %q", cell("-3\n"), cell("-3\n\n"))
	}
}

func TestSheetsColumnName(t *testing.T) {
	for _, tr := range kittest.Load(t, "sheets") {
		if tr.Call.Function != "_column_name_table" {
			continue
		}
		var want map[string]string
		if err := json.Unmarshal(tr.Result, &want); err != nil {
			t.Fatal(err)
		}
		for k, w := range want {
			n, _ := strconv.Atoi(k)
			if got := columnName(n); got != w {
				t.Errorf("columnName(%d) = %q want %q", n, got, w)
			}
		}
		if len(want) != 13 {
			t.Fatalf("vectors %d", len(want))
		}
	}
	if columnName(0) != "" {
		t.Fatal("columnName(0)")
	}
}

func run(t *testing.T, tr *kittest.Transcript) {
	c := New(tr.Client(t), tr)
	var id, key, rng string
	tr.Arg(t, "spreadsheet_id", &id)
	tr.Arg(t, "idempotency_key", &key)
	tr.Arg(t, "sheet_range", &rng)
	var (
		res *wbcrud.Obj
		err error
	)
	switch tr.Call.Function {
	case "append_row":
		res, err = c.AppendRow(context.Background(), kittest.WS, id, values(t, tr, "values"), rng)
	case "upsert_row":
		res, err = c.UpsertRow(context.Background(), kittest.WS, id, values(t, tr, "values"), key, rng)
	default:
		t.Fatalf("%s: function %s", tr.File, tr.Call.Function)
	}
	if err != nil {
		t.Fatalf("%s: %v", tr.File, err)
	}
	tr.CheckResult(t, res)
	tr.Done(t)
}

func TestSheetsAppendTranscripts(t *testing.T) {
	n := 0
	for _, tr := range kittest.Load(t, "sheets") {
		if tr.Call.Function == "append_row" {
			n++
			t.Run(tr.Scenario, func(t *testing.T) { run(t, tr) })
		}
	}
	if n != 9 {
		t.Fatalf("ran %d", n)
	}
}

func TestSheetsUpsertTranscripts(t *testing.T) {
	n := 0
	for _, tr := range kittest.Load(t, "sheets") {
		if tr.Call.Function == "upsert_row" {
			n++
			t.Run(tr.Scenario, func(t *testing.T) { run(t, tr) })
		}
	}
	if n != 5 {
		t.Fatalf("ran %d", n)
	}
}

func TestPathSegmentsEscaped(t *testing.T) {
	var paths []string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.EscapedPath())
		return kittest.Respond(r, 200, `{"updates":{"updatedRange":"x"}}`), nil
	})}, kittest.Static{"w": {"GOOGLE_SHEETS_TOKEN": "tok"}})
	if _, err := c.AppendRow(context.Background(), "w", "id/../x?y", []any{"a"}, "Sheet1/../z"); err != nil {
		t.Fatal(err)
	}
	if paths[0] != "/v4/spreadsheets/id%2F..%2Fx%3Fy/values/Sheet1%2F..%2Fz:append" {
		t.Fatalf("path %q", paths[0])
	}
}

func TestNoSecretInErrors(t *testing.T) {
	const tok = "ctk_sheets_secret_9999"
	st := kittest.Static{"w": {"GOOGLE_SHEETS_TOKEN": tok}}
	for name, rt := range map[string]kittest.RoundTripFunc{
		"transport":   func(r *http.Request) (*http.Response, error) { return nil, errors.New("failed Bearer " + tok) },
		"echoed body": func(r *http.Request) (*http.Response, error) { return kittest.Respond(r, 403, "nope "+tok), nil },
	} {
		t.Run(name, func(t *testing.T) {
			c := New(&http.Client{Transport: rt}, st)
			a, err1 := c.AppendRow(context.Background(), "w", "sid", []any{"a"}, "")
			u, err2 := c.UpsertRow(context.Background(), "w", "sid", []any{"a"}, "k", "")
			if err1 != nil || err2 != nil {
				t.Fatal(err1, err2)
			}
			for _, r := range []*wbcrud.Obj{a, u} {
				b, _ := r.MarshalJSON()
				if strings.Contains(string(b), tok) || !strings.Contains(string(b), `"success":false`) {
					t.Fatalf("result %s", b)
				}
			}
		})
	}
}

func TestRedirectRefused(t *testing.T) {
	calls := 0
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return kittest.Respond(r, 307, "", "Location", "https://evil.example/"), nil
	})}, kittest.Static{"w": {"GOOGLE_SHEETS_TOKEN": "tok"}})
	res, _ := c.AppendRow(context.Background(), "w", "sid", []any{"a"}, "")
	if e, _ := res.Get("error"); e != "HTTP 307: " || calls != 1 {
		t.Fatalf("%v %d", e, calls)
	}
}

func TestTenantTokenIsolation(t *testing.T) {
	var auth []string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		return kittest.Respond(r, 200, `{"updates":{"updatedRange":"S!A1"}}`), nil
	})}, kittest.Static{"a": {"GOOGLE_SHEETS_TOKEN": "tok-a"}, "b": {"GOOGLE_SHEETS_TOKEN": "tok-b"}})
	ctx := context.Background()
	c.AppendRow(ctx, "a", "s", []any{1}, "")
	c.AppendRow(ctx, "b", "s", []any{1}, "")
	res, _ := c.AppendRow(ctx, "c", "s", []any{1}, "")
	if len(auth) != 2 || auth[0] != "Bearer tok-a" || auth[1] != "Bearer tok-b" {
		t.Fatalf("auth %v", auth)
	}
	if e, _ := res.Get("error"); !strings.HasPrefix(e.(string), "Google Sheets not connected") {
		t.Fatalf("c: %v", e)
	}
	if ok, _ := c.IsConnected(ctx, "c"); ok {
		t.Fatal("c connected")
	}
}
