package colgraph

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

type outcome struct {
	Value json.RawMessage `json:"value"`
	Error *string         `json:"error"`
}

type unavail struct {
	Row      map[string]any     `json:"row"`
	ByColumn map[string]outcome `json:"by_column"`
}

type cycleCase struct {
	Name                string              `json:"name"`
	Columns             []json.RawMessage   `json:"columns"`
	Changed             []string            `json:"changed"`
	Refs                map[string][]string `json:"refs"`
	CycleBlocked        []string            `json:"cycle_blocked"`
	TopoOrder           []string            `json:"topo_order"`
	Independent         []string            `json:"independent"`
	Downstream          outcome             `json:"downstream"`
	DownstreamNotOutput outcome             `json:"downstream_not_output"`
	Unavailable         []unavail           `json:"unavailable"`
	Referencing         map[string][]string `json:"referencing"`
}

type cycleFile struct {
	Cases          []cycleCase `json:"cases"`
	ReferenceIndex struct {
		Columns []json.RawMessage `json:"columns"`
		Cases   []struct {
			Ref     string `json:"ref"`
			Indexes []int  `json:"indexes"`
		} `json:"cases"`
	} `json:"reference_indexes"`
}

func loadCycles(t testing.TB) cycleFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/cycles_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f cycleFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func parseCols(t testing.TB, raws []json.RawMessage) []Col {
	t.Helper()
	cols := make([]Col, len(raws))
	for i, r := range raws {
		c, err := ParseCol(r)
		if err != nil {
			t.Fatal(err)
		}
		cols[i] = c
	}
	return cols
}

func ids(cols []Col) []string {
	out := []string{}
	for _, c := range cols {
		if s, ok := c.Get("id").(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func strs(raw json.RawMessage) []string {
	var out []string
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = []string{}
	}
	return out
}

func TestCycleBlockedMatchesVectors(t *testing.T) {
	for _, c := range loadCycles(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := []string{}
			for id := range CycleBlocked(parseCols(t, c.Columns)) {
				got = append(got, id)
			}
			sort.Strings(got)
			want := c.CycleBlocked
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
		})
	}
}

func TestOrderStableAndMatchesPython(t *testing.T) {
	for _, c := range loadCycles(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			cols := parseCols(t, c.Columns)
			got, _ := Order(cols)
			want := c.TopoOrder
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(ids(got), want) {
				t.Fatalf("order got %v want %v", ids(got), want)
			}
			ind := ids(Independent(cols))
			wantInd := c.Independent
			if wantInd == nil {
				wantInd = []string{}
			}
			if !reflect.DeepEqual(ind, wantInd) {
				t.Fatalf("independent got %v want %v", ind, wantInd)
			}
		})
	}
}

func TestRefsByColumnNameAndID(t *testing.T) {
	f := loadCycles(t)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			cols := parseCols(t, c.Columns)
			for _, col := range cols {
				id, ok := col.Get("id").(string)
				if !ok {
					continue
				}
				got := Refs(col)
				want := c.Refs[id]
				if want == nil {
					want = []string{}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("refs(%s) got %v want %v", id, got, want)
				}
				if g, w := ids(Referencing(cols, col)), c.Referencing[id]; !reflect.DeepEqual(nz(g), nz(w)) {
					t.Fatalf("referencing(%s) got %v want %v", id, g, w)
				}
			}
			if c.Downstream.Value != nil {
				got := ids(Downstream(cols, c.Changed, nil))
				if !reflect.DeepEqual(got, strs(c.Downstream.Value)) {
					t.Fatalf("downstream got %v want %v", got, strs(c.Downstream.Value))
				}
				notOut := func(c Col) bool { return c.Get("type") != "output" }
				got = ids(Downstream(cols, c.Changed, notOut))
				if !reflect.DeepEqual(got, strs(c.DownstreamNotOutput.Value)) {
					t.Fatalf("downstream_not_output got %v want %v", got, strs(c.DownstreamNotOutput.Value))
				}
			}
			for _, u := range c.Unavailable {
				for _, col := range cols {
					id, ok := col.Get("id").(string)
					if !ok {
						continue
					}
					got := UnavailableDeps(col, cols, u.Row)
					want := strs(u.ByColumn[id].Value)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("unavailable(%s,%v) got %v want %v", id, u.Row, got, want)
					}
				}
			}
		})
	}
	cols := parseCols(t, f.ReferenceIndex.Columns)
	for _, rc := range f.ReferenceIndex.Cases {
		got := ReferenceIndexes(rc.Ref, cols)
		if !reflect.DeepEqual(nzi(got), nzi(rc.Indexes)) {
			t.Fatalf("reference_indexes(%q) got %v want %v", rc.Ref, got, rc.Indexes)
		}
	}
}

func TestUpstreamIsDirectDependencyIDs(t *testing.T) {
	cols := parseCols(t, []json.RawMessage{
		json.RawMessage(`{"id":"a","name":"A","type":"lead_field"}`),
		json.RawMessage(`{"id":"b","name":"B","type":"ai_formula","prompt":"{a}"}`),
		json.RawMessage(`{"id":"c","name":"C","type":"ai_formula","prompt":"{B} {a} {c} {zzz}"}`),
	})
	if got := Upstream(cols, "c"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
	if got := Upstream(cols, "a"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if got := Upstream(cols, "missing"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestSelfReferenceIsCycle(t *testing.T) {
	cols := parseCols(t, []json.RawMessage{
		json.RawMessage(`{"id":"a","name":"A","type":"ai_formula","prompt":"{a}"}`),
		json.RawMessage(`{"id":"b","name":"B","type":"ai_formula","prompt":"{a}"}`),
		json.RawMessage(`{"id":"e","name":"E","type":"enrichment","condition":"{E} == \"\""}`),
	})
	got := CycleBlocked(cols)
	if !got["a"] || !got["b"] || got["e"] {
		t.Fatalf("got %v", got)
	}
}

func TestOrderFallsBackToConfigOrderOnCycle(t *testing.T) {
	cols := parseCols(t, []json.RawMessage{
		json.RawMessage(`{"id":"x","type":"ai_formula","prompt":"{y}"}`),
		json.RawMessage(`{"id":"y","type":"ai_formula","prompt":"{x}"}`),
	})
	got, err := Order(cols)
	if err == nil {
		t.Fatal("want cycle error")
	}
	if !reflect.DeepEqual(ids(got), []string{"x", "y"}) {
		t.Fatalf("got %v", ids(got))
	}
}

func TestOrderBoundedOnLargeGraph(t *testing.T) {
	const n = 2000
	// Reverse chain: col i depends on col i+1, plus wide fan-in on the last.
	cols := make([]Col, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%04d", i)
		prompt := "x"
		if i+1 < n {
			prompt = fmt.Sprintf("{c%04d} {c%04d}", i+1, n-1)
		}
		cols[i] = MakeCol("id", id, "name", "N "+id, "type", "ai_formula", "prompt", prompt)
	}
	start := time.Now()
	got, err := Order(cols)
	blocked := CycleBlocked(cols)
	_ = Independent(cols)
	if d := Downstream(cols, []string{fmt.Sprintf("c%04d", n-1)}, nil); len(d) != n-1 {
		t.Fatalf("downstream %d", len(d))
	}
	if err != nil || len(blocked) != 0 {
		t.Fatalf("err=%v blocked=%d", err, len(blocked))
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("took %v", el)
	}
	if ids(got)[0] != fmt.Sprintf("c%04d", n-1) || ids(got)[n-1] != "c0000" {
		t.Fatalf("bad order head=%s tail=%s", ids(got)[0], ids(got)[n-1])
	}
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nzi(s []int) []int {
	if s == nil {
		return []int{}
	}
	return s
}
