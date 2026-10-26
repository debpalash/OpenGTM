package vendorcatalog

import (
	"sort"
	"strconv"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// round4 is Python's round(x, 4): exact correctly-rounded decimal rounding.
func round4(x float64) float64 {
	f, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 4, 64), 64)
	if err != nil {
		return x
	}
	return f
}

const (
	noteBase = "worst = every paid provider tried per row; best = cheapest paid hit first. " +
		"Cross-provider cache + confidence early-exit reduce actual spend. "
	noteComplete   = "Catalog estimates are not a vendor invoice or a spend ceiling."
	noteIncomplete = "Unknown provider prices are excluded; these totals are incomplete, not a spend ceiling."
)

// EstimateRunCost estimates a run's spend before it starts. columns gives the
// iteration order of providersByCol (Python dict insertion order); nil sorts
// the column ids.
//
// Float repr matches Python: a column with no paid provider reports worst_usd
// as the int 0 (sum of an empty generator), best_usd as a float.
func EstimateRunCost(numRows int, providersByCol map[string][]string, columns []string) *wbcrud.Obj {
	if columns == nil {
		for c := range providersByCol {
			columns = append(columns, c)
		}
		sort.Strings(columns)
	}
	rows := float64(numRows)
	worst, best := 0.0, 0.0
	breakdown := make([]any, 0, len(columns))
	unknownAll := map[string]struct{}{}
	for _, col := range columns {
		providers := providersByCol[col]
		var unknown []string
		seen := map[string]struct{}{}
		for _, p := range providers {
			if _, ok := vendorIndex[p]; ok || isResearch(p) {
				continue
			}
			if _, ok := providerDeclaredCost(p); ok {
				continue
			}
			unknownAll[p] = struct{}{}
			if _, dup := seen[p]; !dup {
				seen[p] = struct{}{}
				unknown = append(unknown, p)
			}
		}
		var paid []string
		sum, minC := 0.0, 0.0
		for _, p := range providers {
			c := BaseCost(p)
			if c > 0 {
				if len(paid) == 0 || c < minC {
					minC = c
				}
				paid = append(paid, p)
				sum += c
			}
		}
		colWorst := sum * rows
		colBest := minC * rows
		worst += colWorst
		best += colBest
		e := wbcrud.NewObj()
		e.Set("column", col)
		if paid == nil {
			paid = []string{}
		}
		e.Set("paid_providers", paid)
		if len(paid) == 0 {
			e.Set("worst_usd", 0) // Python: int 0
		} else {
			e.Set("worst_usd", round4(colWorst))
		}
		e.Set("best_usd", round4(colBest))
		if unknown == nil {
			unknown = []string{}
		}
		e.Set("unknown_providers", unknown)
		breakdown = append(breakdown, e)
	}
	all := make([]string, 0, len(unknownAll))
	for p := range unknownAll {
		all = append(all, p)
	}
	sort.Strings(all)
	note := noteBase + noteComplete
	if len(all) > 0 {
		note = noteBase + noteIncomplete
	}
	o := wbcrud.NewObj()
	o.Set("rows", numRows)
	o.Set("worst_usd", round4(worst))
	o.Set("best_usd", round4(best))
	o.Set("breakdown", breakdown)
	o.Set("unknown_providers", all)
	o.Set("catalog_complete", len(all) == 0)
	o.Set("note", note)
	return o
}
