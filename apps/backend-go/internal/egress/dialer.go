package egress

import (
	"context"
	"net"
	"time"
)

// DialContext is a dial function with the egress guard applied.
type DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

// NewDialContext returns the dial-time guard NewClient uses, for non-HTTP
// egress such as the SMTP probe: the host is resolved once, the dial is refused
// if ANY resolved address is blocked (loopback, private, link-local, metadata,
// IPv4-mapped forms, odd numeric encodings), and the vetted IP literal is
// dialled, never the hostname, so DNS rebinding cannot swap the target.
// Allowlist hosts are honoured only when appEnv is test or bench.
func NewDialContext(appEnv string, allowHosts []string) DialContext {
	d := &net.Dialer{Timeout: 30 * time.Second}
	return DialContext(guardedDial(allowSet(appEnv, allowHosts), net.DefaultResolver, d.DialContext))
}
