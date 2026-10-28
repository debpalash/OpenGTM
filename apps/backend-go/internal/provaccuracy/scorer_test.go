package provaccuracy

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func readVec(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func val(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	v, err := ParseOrdered(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type accVec struct {
	Normalize []struct {
		Field string          `json:"field"`
		Value json.RawMessage `json:"value"`
		Norm  string          `json:"norm"`
	} `json:"normalize"`
	ValuesMatch []struct {
		Field    string          `json:"field"`
		Got      json.RawMessage `json:"got"`
		Expected json.RawMessage `json:"expected"`
		Match    bool            `json:"match"`
	} `json:"values_match"`
	Freshness []struct {
		AsOf  json.RawMessage `json:"asof"`
		Fresh float64         `json:"fresh"`
	} `json:"freshness"`
	Synthetic struct {
		Golden   json.RawMessage `json:"golden"`
		Outputs  json.RawMessage `json:"outputs"`
		Today    string          `json:"today"`
		Ranking  []Row           `json:"ranking"`
		Verdicts map[string][]struct {
			Case    string  `json:"case"`
			Field   string  `json:"field"`
			Correct bool    `json:"correct"`
			Fresh   float64 `json:"fresh"`
		} `json:"verdicts"`
	} `json:"synthetic"`
	Empty []Row            `json:"empty"`
	Real  map[string][]Row `json:"real"`
}

func TestNormalizeMatchFreshness(t *testing.T) {
	var v accVec
	readVec(t, "accuracy.json", &v)
	for _, c := range v.Normalize {
		if got := Normalize(c.Field, val(t, c.Value)); got != c.Norm {
			t.Errorf("Normalize(%s, %s) = %q, want %q", c.Field, c.Value, got, c.Norm)
		}
	}
	for _, c := range v.ValuesMatch {
		if got := ValuesMatch(c.Field, val(t, c.Got), val(t, c.Expected)); got != c.Match {
			t.Errorf("ValuesMatch(%s, %s, %s) = %v", c.Field, c.Got, c.Expected, got)
		}
	}
	today := day(t, "2026-10-28")
	for _, c := range v.Freshness {
		// libm may differ by one ULP on arm64; the scoring contract is numerical.
		if got := Freshness(val(t, c.AsOf), today); math.Abs(got-c.Fresh) > 1e-15 {
			t.Errorf("Freshness(%s) = %v, want %v", c.AsOf, got, c.Fresh)
		}
	}
}

func TestSyntheticRankingAndVerdicts(t *testing.T) {
	var v accVec
	readVec(t, "accuracy.json", &v)
	golden, err := ParseGolden([]byte(`{"cases":` + string(v.Synthetic.Golden) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	outs, err := ParseOutputs([]byte(`{"outputs":` + string(v.Synthetic.Outputs) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	scores := ScoreOutputs(golden, outs, day(t, v.Synthetic.Today))
	got := Rows(Rank(scores))
	if len(got) != len(v.Synthetic.Ranking) {
		t.Fatalf("%d rows, want %d", len(got), len(v.Synthetic.Ranking))
	}
	for i := range got {
		if got[i] != v.Synthetic.Ranking[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], v.Synthetic.Ranking[i])
		}
	}
	for _, ps := range scores {
		want := v.Synthetic.Verdicts[ps.Provider]
		if len(want) != len(ps.Verdicts) {
			t.Fatalf("%s: %d verdicts, want %d", ps.Provider, len(ps.Verdicts), len(want))
		}
		for i, w := range want {
			g := ps.Verdicts[i]
			if g.CaseID != w.Case || g.Field != w.Field || g.Correct != w.Correct || g.Freshness != w.Fresh {
				t.Errorf("%s verdict %d = %+v, want %+v", ps.Provider, i, g, w)
			}
		}
	}
	if e := Rows(Rank(ScoreOutputs(nil, Outputs{Cases: &OMap{vals: map[string]any{}}}, day(t, "2026-10-28")))); len(e) != 0 || len(v.Empty) != 0 {
		t.Errorf("empty ranking = %v", e)
	}
}

func TestRealDatasetThreeDates(t *testing.T) {
	var v accVec
	readVec(t, "accuracy.json", &v)
	for d, want := range v.Real {
		ranking, err := RunEval(day(t, d))
		if err != nil {
			t.Fatal(err)
		}
		got := Rows(ranking)
		if len(got) != len(want) {
			t.Fatalf("%s: %d rows, want %d", d, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s row %d = %+v, want %+v", d, i, got[i], want[i])
			}
		}
	}
}

func TestFloatMarshalsLikePython(t *testing.T) {
	b, _ := json.Marshal([]Float{1, 0, 0.5, 0.929, 1e-5, 12345678901234567})
	if string(b) != `[1.0,0.0,0.5,0.929,1e-05,1.2345678901234568e+16]` {
		t.Errorf("got %s", b)
	}
}

// TestEmbeddedMatchesRepoData guards the embedded copy against drift from data/provider_eval.
func TestEmbeddedMatchesRepoData(t *testing.T) {
	for _, n := range []string{"golden.json", "recorded_outputs.json"} {
		repo, err := os.ReadFile("../../../../data/provider_eval/" + n)
		if err != nil {
			t.Skip("repo data dir not present")
		}
		emb, _ := dataFS.ReadFile("data/" + n)
		if !strings.EqualFold(string(repo), string(emb)) {
			t.Errorf("%s drifted from data/provider_eval", n)
		}
	}
}
