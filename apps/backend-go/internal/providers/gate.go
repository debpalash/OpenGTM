package providers

import (
	"context"
	"sync/atomic"
)

// LocalGated is implemented by providers that run in process only when the
// GO_LOCAL_PROVIDERS gate lists them (plan 15 WP-PROVIDERS, providers/scrape);
// otherwise their calls keep going to the Python worker.
type LocalGated interface {
	LocalGated()
}

var localGate atomic.Pointer[func(string) bool]

// SetLocalGate installs the GO_LOCAL_PROVIDERS gate (nil serves no gated
// provider in process).
func SetLocalGate(f func(name string) bool) {
	if f == nil {
		localGate.Store(nil)
		return
	}
	localGate.Store(&f)
}

// LocalEnabled reports whether the gate lists name.
func LocalEnabled(name string) bool {
	f := localGate.Load()
	return f != nil && (*f)(name)
}

// InProcess reports whether p runs in process: ungated providers always do,
// gated ones only when LocalEnabled(p.Name()).
func InProcess(p Provider) bool {
	if _, gated := p.(LocalGated); !gated {
		return true
	}
	return LocalEnabled(p.Name())
}

type wsKey struct{}

// WithWorkspace records the calling workspace for providers that salt
// process-wide caches with it (the web search cache).
func WithWorkspace(ctx context.Context, ws string) context.Context {
	return context.WithValue(ctx, wsKey{}, ws)
}

// WorkspaceFrom returns the workspace set by WithWorkspace ("" when none).
func WorkspaceFrom(ctx context.Context) string {
	s, _ := ctx.Value(wsKey{}).(string)
	return s
}
