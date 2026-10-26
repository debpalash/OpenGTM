package providerrun

import (
	"crypto/rand"
	"encoding/hex"
)

// newCallID mints a fresh idempotency id for one Python RPC attempt.
func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "go-" + hex.EncodeToString(b[:])
}
