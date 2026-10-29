// Package vendorcatalog is the Go port of legacy vendor_catalog.py: provider
// economics (base cost, BYOK vs platform, cost scaling) and the pre-run spend
// estimate. Values and float behaviour match Python exactly.
package vendorcatalog

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// researchBaseCost is the worst-case per-cell research spend.
const researchBaseCost = 0.05

// Vendor is one built-in catalog entry.
type Vendor struct {
	Name         string
	BaseCost     float64
	BYOK         bool
	Capabilities []string
	PerPageSize  int
	Notes        string
}

func v(name string, cost float64, caps ...string) Vendor {
	return Vendor{Name: name, BaseCost: cost, BYOK: true, Capabilities: caps, PerPageSize: 25}
}

// vendors is the built-in table in Python's dict order.
var vendors = []Vendor{
	v("hunter_io", 0.04, "email"),
	v("apollo_io", 0.03, "email", "phone", "contact_person", "company_size"),
	v("snovio", 0.03, "email"),
	v("prospeo", 0.02, "email", "phone"),
	v("people_data_labs", 0.03, "email", "phone", "company_size"),
	v("abstract_api", 0.01, "email_verify"),
	v("debounce", 0.008, "email_verify"),
	v("numverify", 0.005, "phone"),
	v("google_maps", 0.005, "phone", "address"),
	v("leadmagic_email", 0.05, "email"),
	v("leadmagic_mobile", 0.05, "mobile_phone", "phone"),
	v("prospeo_mobile", 0.10, "mobile_phone"),
	{Name: "research", BaseCost: researchBaseCost, BYOK: false, Capabilities: []string{"research"},
		PerPageSize: 25, Notes: "flagged by RESEARCH_VENDOR_COST (default off)"},
}

var vendorIndex = func() map[string]*Vendor {
	m := make(map[string]*Vendor, len(vendors))
	for i := range vendors {
		m[vendors[i].Name] = &vendors[i]
	}
	return m
}()

type declaredFn func(name string) (float64, bool)
type flagFn func() bool

var (
	declared atomic.Pointer[declaredFn]
	research atomic.Pointer[flagFn]
)

// SetDeclaredCost installs the lookup for a registered provider's own
// cost_per_lookup (manifest registry); ok=false means not registered or no
// cost. nil removes it.
func SetDeclaredCost(f func(name string) (float64, bool)) {
	if f == nil {
		declared.Store(nil)
		return
	}
	d := declaredFn(f)
	declared.Store(&d)
}

// SetResearchFlag overrides the RESEARCH_VENDOR_COST switch (nil restores the
// environment default).
func SetResearchFlag(f func() bool) {
	if f == nil {
		research.Store(nil)
		return
	}
	d := flagFn(f)
	research.Store(&d)
}

func researchEnabled() bool {
	if p := research.Load(); p != nil {
		return (*p)()
	}
	val := "0"
	if e, ok := os.LookupEnv("RESEARCH_VENDOR_COST"); ok {
		val = e
	}
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "0", "false", "off", "no", "":
		return false
	}
	return true
}

func isResearch(name string) bool { return name == "research" || name == "claygent" }

// providerDeclaredCost mirrors _provider_declared_cost: finite and >= 0.
func providerDeclaredCost(name string) (float64, bool) {
	p := declared.Load()
	if p == nil {
		return 0, false
	}
	c, ok := (*p)(name)
	if !ok || math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		return 0, false
	}
	return c, true
}

// BaseCost is the per-lookup cost: a positive declared cost wins over the
// table; unknown and free providers are 0.
func BaseCost(name string) float64 {
	if isResearch(name) {
		if researchEnabled() {
			return researchBaseCost
		}
		return 0
	}
	if c, ok := providerDeclaredCost(name); ok && c > 0 {
		return c
	}
	if vd, ok := vendorIndex[name]; ok {
		return vd.BaseCost
	}
	return 0
}

// IsPaid reports a positive base cost.
func IsPaid(name string) bool { return BaseCost(name) > 0 }

// HasKnownCost reports a catalog price or a valid declared price.
func HasKnownCost(name string) bool {
	if _, ok := vendorIndex[name]; ok || name == "claygent" {
		return true
	}
	_, ok := providerDeclaredCost(name)
	return ok || nativeFreeProviders[name]
}

// IsBYOK reports BYOK billing (user key); unknown providers are BYOK.
func IsBYOK(name string) bool {
	if vd, ok := vendorIndex[name]; ok {
		return vd.BYOK
	}
	return true
}

// pyInt converts a JSON-ish value like int(x); ok=false when Python would be
// falsy or the value is not convertible.
func pyTruthyInt(x any) (int, bool) {
	switch t := x.(type) {
	case nil:
		return 0, false
	case bool:
		if t {
			return 1, true
		}
		return 0, false
	case int:
		return t, t != 0
	case int64:
		return int(t), t != 0
	case float64:
		return int(t), t != 0
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return int(f), f != 0
		}
	case string:
		if t == "" {
			return 0, false
		}
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n, true
		}
	}
	return 0, false
}

func firstInt(params map[string]any, def int, keys ...string) int {
	for _, k := range keys {
		if n, ok := pyTruthyInt(params[k]); ok {
			return n
		}
	}
	return def
}

// CalculateCost is the cost of one call scaled by operation: search is
// per-page, bulk is per-record, anything else is the base cost.
func CalculateCost(name, operation string, params map[string]any) float64 {
	cost := BaseCost(name)
	if cost <= 0 {
		return 0
	}
	switch operation {
	case "search":
		rows := firstInt(params, 25, "limit", "per_page", "rows")
		per := 25
		if vd, ok := vendorIndex[name]; ok && vd.PerPageSize != 0 {
			per = vd.PerPageSize
		}
		pages := int(math.Ceil(float64(rows) / float64(per)))
		if pages < 1 {
			pages = 1
		}
		return cost * float64(pages)
	case "bulk":
		n := firstInt(params, 1, "count", "records")
		if n < 1 {
			n = 1
		}
		return cost * float64(n)
	}
	return cost
}

// ListVendors lists the built-in vendors in table order.
func ListVendors() []*wbcrud.Obj {
	out := make([]*wbcrud.Obj, 0, len(vendors))
	for _, vd := range vendors {
		o := wbcrud.NewObj()
		o.Set("name", vd.Name)
		o.Set("base_cost", vd.BaseCost)
		o.Set("byok", vd.BYOK)
		o.Set("capabilities", append([]string{}, vd.Capabilities...))
		o.Set("notes", vd.Notes)
		out = append(out, o)
	}
	return out
}
