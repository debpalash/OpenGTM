// Package leadscore ports apps/api/services/leadgen/scoring.py (score_lead, get_tier) and
// enrichment/size_heuristic.normalize_band. The ICP, weights and tiers are embedded from icp.json,
// recorded from apps/api/services/leadgen/config.py.
package leadscore

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

//go:embed icp.json
var icpJSON []byte

type icpConfig struct {
	TargetIndustries        []string `json:"target_industries"`
	TargetCities            []string `json:"target_cities"`
	Tier1Cities             []string `json:"tier1_cities"`
	PreferredSpecialization []string `json:"preferred_specializations"`
}

type tierRange struct {
	Name     string
	Low, Hig float64
}

var (
	icp     icpConfig
	weights map[string]int
	// scoreTiers keeps the SCORE_TIERS dict order (hot, warm, cold, unqualified); get_tier returns the first match.
	scoreTiers []tierRange
)

// ReliabilitySwing is RELIABILITY_SWING: float(os.getenv("SOURCE_RELIABILITY_SWING", "8")), read once at startup.
// An unparseable value falls back to 8 (Python would fail at import).
var ReliabilitySwing = loadSwing()

func loadSwing() float64 {
	if s, ok := os.LookupEnv("SOURCE_RELIABILITY_SWING"); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f
		}
	}
	return 8
}

func init() {
	var doc struct {
		ICP     icpConfig      `json:"ICP"`
		Weights map[string]int `json:"SCORING_WEIGHTS"`
	}
	if err := json.Unmarshal(icpJSON, &doc); err != nil {
		panic("leadscore: icp.json: " + err.Error())
	}
	icp, weights = doc.ICP, doc.Weights

	// Ordered decode of SCORE_TIERS.
	dec := json.NewDecoder(bytes.NewReader(icpJSON))
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil {
		panic(err)
	}
	td := json.NewDecoder(bytes.NewReader(top["SCORE_TIERS"]))
	if _, err := td.Token(); err != nil {
		panic(err)
	}
	for td.More() {
		k, _ := td.Token()
		var r [2]float64
		if err := td.Decode(&r); err != nil {
			panic(err)
		}
		scoreTiers = append(scoreTiers, tierRange{Name: k.(string), Low: r[0], Hig: r[1]})
	}
}
