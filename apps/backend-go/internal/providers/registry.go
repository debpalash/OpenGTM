package providers

import (
	"fmt"
	"sort"
	"sync"
)

var (
	regMu sync.RWMutex
	reg   = map[string]Provider{}
)

// Register adds p under p.Name(). A duplicate name panics (a wiring bug).
func Register(p Provider) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := reg[p.Name()]; dup {
		panic(fmt.Sprintf("providers: %q registered twice", p.Name()))
	}
	reg[p.Name()] = p
}

// Get returns the provider registered under name. Provider packages register
// themselves from init, so importers blank-import them (providers/contact).
func Get(name string) (Provider, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	p, ok := reg[name]
	return p, ok
}

// Names lists the registered provider names, sorted.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(reg))
	for n := range reg {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
