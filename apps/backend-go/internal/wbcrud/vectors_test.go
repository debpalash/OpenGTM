package wbcrud

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// vectors mirrors testdata/wbcrud/column_vectors.json, generated from the real
// Python by legacy/python/scripts/gen_column_vectors.py.
type vectors struct {
	Deps []struct {
		Name        string            `json:"name"`
		Columns     []json.RawMessage `json:"columns"`
		Refs        [][]string        `json:"refs"`
		Referencing [][]any           `json:"referencing"`
		Blocked     []string          `json:"blocked"`
	} `json:"deps"`
	Renames []struct {
		Name   string            `json:"name"`
		Before []json.RawMessage `json:"before"`
		After  []json.RawMessage `json:"after"`
		Broken []any             `json:"broken"`
		Guard  *guardVec         `json:"guard"`
	} `json:"renames"`
	Cycles []struct {
		Name   string            `json:"name"`
		Before []json.RawMessage `json:"before"`
		After  []json.RawMessage `json:"after"`
		Guard  *guardVec         `json:"guard"`
	} `json:"cycles"`
	Normalize []struct {
		Name   string           `json:"name"`
		Input  string           `json:"input"`
		Column string           `json:"column"`
		Errors []map[string]any `json:"errors"`
	} `json:"normalize"`
	Views []struct {
		Name         string           `json:"name"`
		Input        string           `json:"input"`
		Config       string           `json:"config"`
		Errors       []map[string]any `json:"errors"`
		Guard        *guardVec        `json:"guard"`
		GuardUnknown *guardVec        `json:"guard_unknown"`
	} `json:"views"`
	ViewsKnown []string `json:"views_known"`
	Stored     []struct {
		Name     string `json:"name"`
		Stored   string `json:"stored"`
		Expected string `json:"expected"`
	} `json:"stored"`
	Patch []struct {
		Name    string `json:"name"`
		Before  string `json:"before"`
		Changes string `json:"changes"`
		Merged  string `json:"merged"`
	} `json:"patch"`
	Locs []struct {
		Name   string  `json:"name"`
		Input  string  `json:"input"`
		Prefix []any   `json:"prefix"`
		Locs   [][]any `json:"locs"`
		View   bool    `json:"view"`
	} `json:"locs"`
}

type guardVec struct {
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../testdata/wbcrud/column_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func rawColumns(t *testing.T, raws []json.RawMessage) []Column {
	t.Helper()
	b, err := json.Marshal(raws)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := ParseColumns(b)
	if err != nil {
		t.Fatal(err)
	}
	return cols
}

func colIDs(cols []Column) []any {
	out := []any{}
	for _, c := range cols {
		out = append(out, c.Get("id"))
	}
	return out
}

func sameJSON(t *testing.T, got any, want any) bool {
	t.Helper()
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	var g, w any
	if err := json.Unmarshal(gb, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wb, &w); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(g, w)
}

func compact(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}
