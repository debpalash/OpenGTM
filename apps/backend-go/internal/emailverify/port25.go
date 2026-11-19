package emailverify

import (
	"sync"
	"time"
)

// Port 25 is blocked outbound by most cloud providers and many home networks. A single refused or silent mail
// server proves nothing (it may be down, or may refuse us), but a run of DIFFERENT mail servers that all fail to
// answer while none has answered in the same window means this machine cannot reach port 25 at all. Then every
// address would end "unknown", and the honest thing is to say so and point at a verification provider key.
const (
	port25Window   = 15 * time.Minute
	port25MinHosts = 4
)

// connTrack remembers, per mail server, when we last reached it and when we last failed to connect to it.
type connTrack struct {
	mu     sync.Mutex
	okAt   map[string]time.Time
	failAt map[string]time.Time
}

func (c *connTrack) note(host string, ok bool, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.okAt == nil {
		c.okAt, c.failAt = map[string]time.Time{}, map[string]time.Time{}
	}
	if ok {
		c.okAt[host] = now
		delete(c.failAt, host)
		return
	}
	c.failAt[host] = now
}

// blocked: at least port25MinHosts distinct servers failed to connect inside the window and none connected inside it.
func (c *connTrack) blocked(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.okAt {
		if now.Sub(t) < port25Window {
			return false
		}
	}
	n := 0
	for _, t := range c.failAt {
		if now.Sub(t) < port25Window {
			n++
		}
	}
	return n >= port25MinHosts
}

// Port25Blocked reports whether this machine looks unable to reach any mail server on port 25.
func (d Deps) Port25Blocked() bool { return d.memo().conn.blocked(d.now()) }
