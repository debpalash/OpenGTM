package jobkit

import "github.com/debpalash/OpenGTM/apps/server/internal/queue"

// Declare registers a Go executor for jobType that stays Python-owned: the
// handler is built into `opengtm worker`, but the type is claimed by Go only
// after `opengtm routes set <type> go`, and `routes list` shows it as a
// switchable python default. Migrations seed no route for these types.
//
// Only call it for a type whose Go executor has a passing Python/Go parity
// proof (see tests/test_*_go_parity_pg.py); a type without one must not be
// registered at all.
func Declare(name, jobType string, register queue.Registrar) {
	queue.AddRegistrar(name, register)
	queue.DeclareSwitchable(jobType)
}
