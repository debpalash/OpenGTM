package contact

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"

// ReacherAvailable is ReacherVerifier.is_available(): Reacher is enabled, its
// (global) URL passes the allowlist and the process-global circuit breaker is
// closed. The email cascade asks this before putting Reacher at the front of its
// chain, since Enrich only reports availability after the call.
func ReacherAvailable(in providers.Input) bool {
	p, ok := providers.Get("reacher")
	if !ok {
		return false
	}
	r, ok := p.(*reacher)
	return ok && r.available(in)
}
