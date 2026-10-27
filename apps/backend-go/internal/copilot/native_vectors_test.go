package copilot

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
)

// Vectors recorded from Python by testdata/record_native.py (plan 15 WP-COPILOT-CORE).

func readVectors(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestSystemPromptMatchesPython(t *testing.T) {
	var f struct {
		Cases []struct {
			Name   string          `json:"name"`
			Stats  json.RawMessage `json:"stats"`
			Prompt string          `json:"prompt"`
		} `json:"cases"`
	}
	readVectors(t, "native_prompt.json", &f)
	for _, c := range f.Cases {
		var st *PromptStats
		var err error
		switch string(c.Stats) {
		case "null":
		case `"boom"`:
			err = os.ErrInvalid
		default:
			var raw struct {
				Total  int64            `json:"total"`
				ByTier map[string]int64 `json:"by_tier"`
			}
			if e := json.Unmarshal(c.Stats, &raw); e != nil {
				t.Fatal(e)
			}
			st = &PromptStats{Total: raw.Total, ByTier: raw.ByTier}
		}
		if got := RenderSystemPrompt(st, err); got != c.Prompt {
			t.Errorf("%s: prompt differs (len %d vs %d)", c.Name, len(got), len(c.Prompt))
		}
	}
}

func TestOpenUISpecMatchesData(t *testing.T) {
	root := testutil.LegacyPythonDir(t) // the repository checkout is only guaranteed next to legacy/python
	b, err := os.ReadFile(strings.TrimSuffix(root, "/legacy/python") + "/data/openui_system_prompt.txt")
	if err != nil {
		t.Skipf("data/openui_system_prompt.txt unavailable: %v", err)
	}
	if string(b) != openUISpec {
		t.Fatal("internal/copilot/openui_system_prompt.txt differs from data/openui_system_prompt.txt; copy it again")
	}
}

func TestDescribeActionMatchesPython(t *testing.T) {
	var f struct {
		Cases []struct {
			Name        string          `json:"name"`
			Args        json.RawMessage `json:"args"`
			Description string          `json:"description"`
		} `json:"cases"`
	}
	readVectors(t, "native_describe.json", &f)
	for _, c := range f.Cases {
		av, err := canonjson.Decode(c.Args) // as the turn decodes tool-call arguments
		if err != nil {
			t.Fatal(err)
		}
		args := av.(map[string]any)
		got, ok := DescribeAction(c.Name, args)
		if !ok || got != c.Description {
			t.Errorf("%s %v: got %q ok=%v, want %q", c.Name, args, got, ok, c.Description)
		}
	}
	if _, ok := DescribeAction("execute_plan", map[string]any{"plan_id": "p"}); ok {
		t.Error("execute_plan must be described by the worker (server-stored plan)")
	}
}

// briefDeviations are the Python defects fixed in Go (see accountbrief.go): the expected company types /
// geographies replace Python's, and missing_fields, complete and brief_id follow from them.
var briefDeviations = map[string]struct{ types, geos []any }{
	"build me a list of 30 logistics companies in Mumbai which are hiring sales": {types: []any{"logistics"}},
	"build a list of 12 healthcare companies in Delhi":                           {types: []any{"healthcare"}},
	"find 15 companies":                            {types: []any{}},
	"please find 20 retail companies in São Paulo": {geos: []any{"São Paulo"}},
}

func TestAccountBriefMatchesPython(t *testing.T) {
	var f struct {
		Briefs []struct {
			Query   string          `json:"query"`
			Brief   json.RawMessage `json:"brief"`
			Extract json.RawMessage `json:"extract"`
		} `json:"briefs"`
	}
	readVectors(t, "native_brief.json", &f)
	for _, c := range f.Briefs {
		var want map[string]any
		if err := json.Unmarshal(c.Brief, &want); err != nil {
			t.Fatal(err)
		}
		wantExtract := string(c.Extract) != "null"
		if dv, ok := briefDeviations[c.Query]; ok {
			if dv.types != nil {
				want["company_types"] = dv.types
			}
			if dv.geos != nil {
				want["geographies"] = dv.geos
			}
			missing := []any{}
			if want["requested_count"].(float64) == 0 {
				missing = append(missing, "requested_count")
			}
			if len(want["company_types"].([]any)) == 0 {
				missing = append(missing, "company_type")
			}
			want["missing_fields"], want["complete"] = missing, len(missing) == 0
			want["brief_id"] = AccountBriefID(int64(want["requested_count"].(float64)), want["company_types"].([]any),
				want["geographies"].([]any), want["technologies"].([]any), want["hiring_roles"].([]any), []any{})
			wantExtract = wantExtract && len(missing) == 0
		}
		got := BuildAccountBrief(c.Query)
		gb, _ := json.Marshal(got)
		var gm map[string]any
		_ = json.Unmarshal(gb, &gm)
		if !reflect.DeepEqual(gm, want) {
			t.Errorf("%q:\n got %s\nwant %v", c.Query, gb, want)
		}
		if _, ok := briefDeviations[c.Query]; !ok {
			ps, err := objectPairs(c.Brief)
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, p := range ps {
				keys = append(keys, p.k)
			}
			if !reflect.DeepEqual(got.Keys(), keys) {
				t.Errorf("%q: key order %v, want %v", c.Query, got.Keys(), keys)
			}
		}
		if (ExtractAccountDiscovery(c.Query) != nil) != wantExtract {
			t.Errorf("%q: extract = %v, want %v", c.Query, !wantExtract, wantExtract)
		}
	}
}

func TestTrackingRequestMatchesPython(t *testing.T) {
	var f struct {
		Tracking []struct {
			Text    string `json:"text"`
			Request *struct {
				OriginalQuery string   `json:"original_query"`
				Cadence       string   `json:"cadence"`
				SignalTypes   []string `json:"signal_types"`
			} `json:"request"`
		} `json:"tracking"`
	}
	readVectors(t, "native_brief.json", &f)
	for _, c := range f.Tracking {
		got := ExtractTrackingRequest(c.Text)
		if (got == nil) != (c.Request == nil) {
			t.Errorf("%q: got %+v, want %+v", c.Text, got, c.Request)
			continue
		}
		if got != nil && (got.OriginalQuery != c.Request.OriginalQuery || got.Cadence != c.Request.Cadence ||
			!reflect.DeepEqual(got.SignalTypes, c.Request.SignalTypes)) {
			t.Errorf("%q: got %+v, want %+v", c.Text, got, c.Request)
		}
	}
}
