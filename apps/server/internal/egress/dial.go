package egress

import (
	"context"
	"net"
	"net/url"
)

// DialGuarded opens a TCP connection to addr ("host:port") the way the client
// opens its own: the destination guard checks the host name and literal
// spellings, every resolved address is validated, the connection goes to a
// validated address (so DNS rebinding cannot swap the target), and the
// configured egress proxy, if any, carries the connection. It is for callers
// that must tunnel opaque TLS on behalf of a plugin (the process runtime's
// CONNECT proxy); everything with a URL should go through Do, which also
// applies robots.txt, rate limits and size caps.
func (c *Client) DialGuarded(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, blocked("only TCP connections are allowed")
	}
	if err := c.guard.CheckURL(&url.URL{Scheme: "https", Host: addr, Path: "/"}); err != nil {
		return nil, err
	}
	return c.dial(ctx, network, addr)
}
