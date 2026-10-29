package wbcrud

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// cursorVectors mirrors testdata/wbcrud/cursor_vectors.json, generated from the
// real Python encoders/decoders by legacy/python/scripts/gen_cursor_vectors.py.
type cursorVectors struct {
	RowEncode []struct {
		Position int    `json:"position"`
		ID       int64  `json:"id"`
		Cursor   string `json:"cursor"`
	} `json:"row_encode"`
	RowDecode []decodeVec `json:"row_decode"`
	QueryEnc  []struct {
		Values []any  `json:"values"`
		ID     int64  `json:"id"`
		Scope  string `json:"scope"`
		Cursor string `json:"cursor"`
	} `json:"query_encode"`
	QueryDecode []struct {
		decodeVec
		ExpectedTerms int    `json:"expected_terms"`
		Scope         string `json:"scope"`
	} `json:"query_decode"`
	V2Expect struct {
		Row    int64  `json:"row"`
		Scope  string `json:"scope"`
		Digest string `json:"digest"`
	} `json:"v2_expect"`
	ConnEncode []struct {
		CreatedAt string `json:"created_at"`
		ID        string `json:"id"`
		Cursor    string `json:"cursor"`
		UTC       string `json:"utc"`
	} `json:"conn_encode"`
	ConnDecode []decodeVec `json:"conn_decode"`
}

type decodeVec struct {
	Name   string          `json:"name"`
	Cursor string          `json:"cursor"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Status int             `json:"status"`
	Detail string          `json:"detail"`
}

func loadCursorVectors(t *testing.T) cursorVectors {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/wbcrud/cursor_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v cursorVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func wantGuard(t *testing.T, name string, g *GuardError, ok bool, status int, detail string) {
	t.Helper()
	if ok {
		if g != nil {
			t.Fatalf("%s: unexpected guard %v", name, g)
		}
		return
	}
	if g == nil || g.Status != status || g.Detail != detail {
		t.Fatalf("%s: guard = %v, want %d %q", name, g, status, detail)
	}
}

func TestCursorsDecodePythonIssued(t *testing.T) {
	v := loadCursorVectors(t)
	for _, c := range v.RowDecode {
		pos, id, g := DecodeRowCursor(c.Cursor)
		wantGuard(t, "row/"+c.Name, g, c.OK, c.Status, c.Detail)
		if c.OK {
			var want []int64
			_ = json.Unmarshal(c.Result, &want)
			if int64(pos) != want[0] || id != want[1] {
				t.Fatalf("row/%s: got (%d,%d) want %v", c.Name, pos, id, want)
			}
		}
	}
	for _, c := range v.QueryDecode {
		raw, g := DecodeQueryCursorRaw(c.Cursor, c.ExpectedTerms, c.Scope)
		wantGuard(t, "query/"+c.Name, g, c.OK, c.Status, c.Detail)
		if !c.OK {
			continue
		}
		var want struct {
			Values      []any `json:"values"`
			NeedsLookup bool  `json:"needs_lookup"`
		}
		_ = json.Unmarshal(c.Result, &want)
		if want.NeedsLookup {
			if !raw.Lookup || raw.RowID != v.V2Expect.Row || raw.Digest != v.V2Expect.Digest {
				t.Fatalf("query/%s: raw = %+v, want lookup row %d", c.Name, raw, v.V2Expect.Row)
			}
			continue
		}
		if raw.Lookup || !sameValues(raw.Values, want.Values) {
			t.Fatalf("query/%s: values %v want %v", c.Name, raw.Values, want.Values)
		}
		vals, g := DecodeQueryCursor(c.Cursor, c.ExpectedTerms, c.Scope)
		if g != nil || !sameValues(vals, want.Values) {
			t.Fatalf("query/%s: DecodeQueryCursor = %v %v", c.Name, vals, g)
		}
	}
	for _, c := range v.ConnDecode {
		at, id, g := DecodeConnectorRunCursor(c.Cursor)
		wantGuard(t, "conn/"+c.Name, g, c.OK, c.Status, c.Detail)
		if c.OK {
			var want struct {
				UTC string `json:"utc"`
				ID  string `json:"id"`
			}
			_ = json.Unmarshal(c.Result, &want)
			wt, _ := time.Parse(time.RFC3339Nano, want.UTC)
			if !at.Equal(wt) || id != want.ID || at.Location() != time.UTC {
				t.Fatalf("conn/%s: got %v %q want %v %q", c.Name, at, id, wt, want.ID)
			}
		}
	}
}

// sameValues compares decoded cursor values to JSON-decoded expectations.
func sameValues(got, want []any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		g, w := got[i], want[i]
		if f, ok := w.(float64); ok {
			switch n := g.(type) {
			case int64:
				if float64(n) != f {
					return false
				}
				continue
			case float64:
				if n != f {
					return false
				}
				continue
			}
			return false
		}
		if !reflect.DeepEqual(g, w) {
			return false
		}
	}
	return true
}

// Cursors Go issues are byte-identical to the ones Python issues for the same
// inputs, so Python decodes them (and vice versa).
func TestEncodedCursorsDecodeInPython(t *testing.T) {
	v := loadCursorVectors(t)
	for _, c := range v.RowEncode {
		if got := EncodeRowCursor(c.Position, c.ID); got != c.Cursor {
			t.Fatalf("row (%d,%d): %s want %s", c.Position, c.ID, got, c.Cursor)
		}
	}
	for i, c := range v.QueryEnc {
		vals := make([]any, len(c.Values))
		for j, x := range c.Values {
			if f, ok := x.(float64); ok {
				vals[j] = int64(f)
			} else {
				vals[j] = x
			}
		}
		if got := EncodeQueryCursor(vals, c.ID, c.Scope); got != c.Cursor {
			t.Fatalf("query[%d]: %s\nwant %s", i, got, c.Cursor)
		}
	}
	for _, c := range v.ConnEncode {
		at, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		if got := EncodeConnectorRunCursor(at, c.ID); got != c.Cursor {
			t.Fatalf("conn %s: %s want %s", c.ID, got, c.Cursor)
		}
	}
}

func TestCursorsRoundTripGoIssued(t *testing.T) {
	long := strings.Repeat("é", 400)
	vals := []any{long, int64(3), int64(99)}
	scope := CursorScope(nil, []OrderTerm{{Expr: "a"}})
	cur := EncodeQueryCursor(vals, 99, scope)
	raw, g := DecodeQueryCursorRaw(cur, 3, scope)
	if g != nil || !raw.Lookup || raw.RowID != 99 || raw.Digest != ValuesDigest(vals) {
		t.Fatalf("v2 round trip: %+v %v", raw, g)
	}
	short := EncodeQueryCursor([]any{"a", int64(1), int64(2)}, 2, scope)
	got, g := DecodeQueryCursor(short, 3, scope)
	if g != nil || !sameValues(got, []any{"a", float64(1), float64(2)}) {
		t.Fatalf("v1 round trip: %v %v", got, g)
	}
}

func TestCursorsRejectTamperedAndWrongScope(t *testing.T) {
	const rowMsg = "Invalid workbook row cursor"
	const restart = "Invalid workbook row cursor; restart pagination"
	scope := CursorScope(strp("v1"), []OrderTerm{{Expr: "x"}, {Expr: "position"}, {Expr: "id"}})
	other := CursorScope(strp("v2"), []OrderTerm{{Expr: "x"}, {Expr: "position"}, {Expr: "id"}})
	if scope == other || len(scope) != 64 {
		t.Fatalf("scope not distinct sha256 hex: %q %q", scope, other)
	}
	long := EncodeQueryCursor([]any{strings.Repeat("x", 700), int64(1), int64(2)}, 2, scope)
	if _, g := DecodeQueryCursorRaw(long, 3, other); g == nil || g.Status != 400 || g.Detail != restart {
		t.Fatalf("wrong scope: %v", g)
	}
	good := EncodeQueryCursor([]any{"a", int64(1), int64(2)}, 2, scope)
	for _, bad := range []string{good[:len(good)-3], "!" + good, good + "$"} {
		if _, g := DecodeQueryCursorRaw(bad, 3, scope); g == nil || g.Status != 400 {
			t.Fatalf("tampered %q accepted", bad)
		}
	}
	if _, g := DecodeQueryCursorRaw(good, 4, scope); g == nil || g.Detail != rowMsg {
		t.Fatalf("shape mismatch: %v", g)
	}
	if _, _, g := DecodeRowCursor("garbage!"); g == nil || g.Status != 400 || g.Detail != rowMsg {
		t.Fatalf("row garbage: %v", g)
	}
	if _, _, g := DecodeConnectorRunCursor(good); g == nil || g.Detail != "Invalid connector run cursor" {
		t.Fatalf("connector garbage: %v", g)
	}
}

func strp(s string) *string { return &s }
