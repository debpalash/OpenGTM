package entities

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// vectors is testdata/entities/merge_vectors.json, generated from the Python reference by
// legacy/python/scripts/gen_entity_merge_vectors.py.
type vectors struct {
	Match struct {
		Companies [][]string           `json:"companies"`
		Domains   [][]string           `json:"domains"`
		Phones    [][]string           `json:"phones"`
		Pairs     [][]float64          `json:"pairs"`
		Leads     []map[string]*string `json:"leads"`
	} `json:"match"`
	Seed         map[string][]json.RawMessage `json:"seed"`
	SeedOrder    []string                     `json:"seed_order"`
	PostMergeSQL string                       `json:"post_merge_sql"`
	DumpSQL      string                       `json:"dump_sql"`
	Python       struct {
		Merge                json.RawMessage            `json:"merge"`
		AfterMerge           map[string]json.RawMessage `json:"after_merge"`
		AfterLateObservation map[string]json.RawMessage `json:"after_late_observation"`
		Split                json.RawMessage            `json:"split"`
		AfterSplit           map[string]json.RawMessage `json:"after_split"`
		Errors               map[string]struct {
			Error string `json:"error"`
		} `json:"errors"`
	} `json:"python"`
}

func loadVectors(t testing.TB) vectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "entities", "merge_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
