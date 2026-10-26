//go:build integration

package leadswrite_test

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
)

type vector struct {
	Class  string  `json:"class"`
	Column string  `json:"column"`
	Raw    string  `json:"raw"`
	OK     bool    `json:"ok"`
	Stored *string `json:"stored"`
}

func loadVectors(t testing.TB) []vector {
	t.Helper()
	b, err := os.ReadFile("../../testdata/leadwrite_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(b, &vs); err != nil {
		t.Fatal(err)
	}
	return vs
}

func body(t testing.TB, raw string) any {
	t.Helper()
	switch raw {
	case "NaN":
		return math.NaN()
	case "Infinity":
		return math.Inf(1)
	}
	d := json.NewDecoder(bytes.NewReader([]byte(raw)))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
