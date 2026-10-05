package enrich

import (
	"math"
	"math/big"
	"strconv"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

// vendors is the built-in paid-provider table of
// apps/api/services/workbook/vendor_catalog.py (VENDORS). A manifest's own
// cost_per_lookup wins when it is positive; otherwise this table applies, and
// anything else is free.
var vendors = map[string]float64{
	"hunter_io":        0.04,
	"apollo_io":        0.03,
	"snovio":           0.03,
	"prospeo":          0.02,
	"people_data_labs": 0.03,
	"abstract_api":     0.01,
	"debounce":         0.008,
	"numverify":        0.005,
	"google_maps":      0.005,
	"leadmagic_email":  0.05,
	"leadmagic_mobile": 0.05,
	"prospeo_mobile":   0.10,
}

// declaredCost is vendor_catalog._provider_declared_cost: the connector's own
// price when it is a finite, non-negative number.
func declaredCost(p *manifest.Plugin) (float64, bool) {
	if p == nil || p.Provider == nil {
		return 0, false
	}
	c := p.Provider.CostPerLookup
	if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		return 0, false
	}
	return c, true
}

// baseCost is vendor_catalog.base_cost. p is nil for a name that is not a
// loaded connector. The research/claygent entry is flag-gated off by default
// in Python and cannot be a connector, so it is not modelled.
func baseCost(name string, p *manifest.Plugin) float64 {
	if c, ok := declaredCost(p); ok && c > 0 {
		return c
	}
	return vendors[name]
}

func isPaid(name string, p *manifest.Plugin) bool { return baseCost(name, p) > 0 }

// hasKnownCost is vendor_catalog.has_known_cost.
func hasKnownCost(name string, p *manifest.Plugin) bool {
	if _, ok := vendors[name]; ok || name == "claygent" {
		return true
	}
	_, ok := declaredCost(p)
	return ok
}

// microUSD is int((Decimal(str(cost)) * 1000000).to_integral_value(ROUND_CEILING)).
func microUSD(cost float64) int64 {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(cost, 'g', -1, 64))
	if !ok {
		return 0
	}
	r.Mul(r, big.NewRat(1000000, 1))
	return ratCeil(r)
}

func ratCeil(r *big.Rat) int64 {
	q, m := new(big.Int).DivMod(r.Num(), r.Denom(), new(big.Int))
	if m.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

func ratFloor(r *big.Rat) int64 {
	return new(big.Int).Div(r.Num(), r.Denom()).Int64()
}
