package queue

import (
	"slices"
	"sync"
)

var (
	switchableMu sync.Mutex
	switchable   = map[string]struct{}{}
	executors    = map[string]struct{}{}
)

// DeclareExecutor records that this binary has a Go executor for jobType,
// whether or not Python also implements it. `opengtm routes set <type> go`
// uses it to refuse a route that nothing would claim, and `routes list` and
// `doctor` to flag one. Types declared with DeclareSwitchable are executors
// too. Packages call it from init(), next to AddRegistrar.
func DeclareExecutor(jobType string) {
	switchableMu.Lock()
	defer switchableMu.Unlock()
	executors[jobType] = struct{}{}
}

// HasExecutor reports whether this binary can execute jobType in Go.
func HasExecutor(jobType string) bool {
	switchableMu.Lock()
	defer switchableMu.Unlock()
	_, ok := executors[jobType]
	return ok
}

// Executors lists the job types this binary can execute, in a stable order.
func Executors() []string {
	switchableMu.Lock()
	defer switchableMu.Unlock()
	out := make([]string, 0, len(executors))
	for t := range executors {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// DeclareSwitchable records that this binary has a Go executor for jobType, so
// an operator may route it with `opengtm routes set <type> go` even though no
// route row exists yet. Types stay Python-owned until they are routed; the
// declaration only makes the option visible (`opengtm routes list`) and keeps
// the worker's "not routed" startup warning quiet for an intentional default.
// Packages call it from init(), next to AddRegistrar.
func DeclareSwitchable(jobType string) {
	switchableMu.Lock()
	defer switchableMu.Unlock()
	switchable[jobType] = struct{}{}
	executors[jobType] = struct{}{}
}

// Switchable lists the declared job types in a stable order.
func Switchable() []string {
	switchableMu.Lock()
	defer switchableMu.Unlock()
	out := make([]string, 0, len(switchable))
	for t := range switchable {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

func isSwitchable(jobType string) bool {
	switchableMu.Lock()
	defer switchableMu.Unlock()
	_, ok := switchable[jobType]
	return ok
}
