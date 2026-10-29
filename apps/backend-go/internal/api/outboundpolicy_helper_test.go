package api

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// testRegistry is the settings registry production wires into Deps. Outbound secret-ref policies
// fail closed without one (final review L9), so route tests that accept refs need it.
func testRegistry(t testing.TB) *settings.Registry {
	t.Helper()
	reg, err := settings.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}
