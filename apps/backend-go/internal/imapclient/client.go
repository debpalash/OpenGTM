// Package imapclient is the inbound-mailbox client behind the bounce poller
// (Python ImapClient protocol), over github.com/emersion/go-imap/v2. It is
// hardened for customer-configured hosts: the caller supplies an SSRF-guarded
// dial (egress.NewDialContext) and a nil dial is refused; certificates are
// always verified; STARTTLS is mandatory unless the host is allow-listed for
// tests; fetches are size-bounded; every call is bounded by ctx and a timeout;
// the password never appears in an error.
//
// It only fetches raw bytes. Parsing belongs to internal/dsnparse.
package imapclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// MaxFetchBytes caps one fetched message; larger answers as an empty fetch.
const MaxFetchBytes = 5 << 20

// DefaultTimeout bounds each call when no timeout is given.
const DefaultTimeout = 60 * time.Second

// Client is the Python ImapClient protocol.
type Client interface {
	Connect(ctx context.Context) error
	Select(ctx context.Context, folder string) (uidValidity string, err error)
	SearchUnseen(ctx context.Context, since time.Time) ([]string, error)
	Fetch(ctx context.Context, uid string) ([]byte, error)
	MarkSeen(ctx context.Context, uid string) error
	Logout()
}

// Options are test-only escape hatches. Production wiring (New) leaves them
// empty: system roots, and no plaintext host.
type Options struct {
	RootCAs    *x509.CertPool
	PlainHosts []string
}

type client struct {
	cfg     outreachcfg.IMAP
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	timeout time.Duration
	opts    Options

	mu   sync.Mutex
	c    *imapclient.Client
	conn net.Conn
}

// New builds the production client (no test hatches).
func New(cfg outreachcfg.IMAP, dial func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration) Client {
	return NewWithOptions(cfg, dial, timeout, Options{})
}

// NewWithOptions is for tests only.
func NewWithOptions(cfg outreachcfg.IMAP, dial func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration, opts Options) Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &client{cfg: cfg, dial: dial, timeout: timeout, opts: opts}
}

var errNotConnected = errors.New("imap: not connected")

const minRedactLen = 4

func (c *client) redact(err error) error {
	if err == nil {
		return nil
	}
	pw := string(c.cfg.Password)
	if len(pw) < minRedactLen {
		return err
	}
	msg := err.Error()
	s := strings.ReplaceAll(msg, pw, "[redacted]")
	s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte(pw)), "[redacted]")
	if s == msg {
		return err
	}
	return &redactedError{msg: s, err: err}
}

type redactedError struct {
	msg string
	err error
}

func (r *redactedError) Error() string { return r.msg }
func (r *redactedError) Unwrap() error { return r.err }

func (c *client) plainAllowed(host string) bool {
	for _, h := range c.opts.PlainHosts {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}

// run executes fn under ctx and the per-call timeout: when either fires the
// connection is closed so any blocked read or write returns.
func (c *client) run(ctx context.Context, conn func() net.Conn, fn func() error) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			if nc := conn(); nc != nil {
				nc.Close()
			}
		case <-stop:
		}
	}()
	err := fn()
	close(stop)
	<-done
	if cerr := ctx.Err(); cerr != nil && err != nil {
		return fmt.Errorf("imap: %w", cerr)
	}
	return err
}

func (c *client) Connect(ctx context.Context) error {
	if c.dial == nil {
		return errors.New("imap: refusing to connect without a guarded dial function")
	}
	if c.cfg.Host == "" {
		return errors.New("imap: host not configured")
	}
	c.Logout()
	var raw net.Conn
	err := c.run(ctx, func() net.Conn { c.mu.Lock(); defer c.mu.Unlock(); return c.conn }, func() error {
		cctx, cancel := context.WithTimeout(ctx, c.timeout)
		defer cancel()
		nc, err := c.dial(cctx, "tcp", net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.Port)))
		if err != nil {
			return fmt.Errorf("imap: dial: %w", err)
		}
		raw = nc
		c.mu.Lock()
		c.conn = nc
		c.mu.Unlock()
		return c.handshake(ctx, nc)
	})
	if err != nil {
		c.Logout()
		if raw != nil {
			raw.Close()
		}
		return c.redact(err)
	}
	return nil
}

func (c *client) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: c.cfg.Host, RootCAs: c.opts.RootCAs, MinVersion: tls.VersionTLS12}
}

func (c *client) handshake(ctx context.Context, nc net.Conn) error {
	var ic *imapclient.Client
	switch {
	case c.cfg.UseSSL:
		tc := tls.Client(nc, c.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("imap: tls: %w", err)
		}
		c.setConn(tc)
		ic = imapclient.New(tc, nil)
	case c.plainAllowed(c.cfg.Host):
		ic = imapclient.New(nc, nil)
	default:
		var err error
		ic, err = imapclient.NewStartTLS(nc, &imapclient.Options{TLSConfig: c.tlsConfig()})
		if err != nil {
			return fmt.Errorf("imap: starttls required: %w", err)
		}
	}
	if err := ic.WaitGreeting(); err != nil {
		ic.Close()
		return fmt.Errorf("imap: greeting: %w", err)
	}
	if err := ic.Login(c.cfg.User, string(c.cfg.Password)).Wait(); err != nil {
		ic.Close()
		return fmt.Errorf("imap: login failed: %w", err)
	}
	c.mu.Lock()
	c.c = ic
	c.mu.Unlock()
	return nil
}

func (c *client) setConn(nc net.Conn) {
	c.mu.Lock()
	c.conn = nc
	c.mu.Unlock()
}

func (c *client) live() (*imapclient.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c == nil {
		return nil, errNotConnected
	}
	return c.c, nil
}

func (c *client) call(ctx context.Context, fn func(ic *imapclient.Client) error) error {
	ic, err := c.live()
	if err != nil {
		return err
	}
	return c.redact(c.run(ctx, func() net.Conn { c.mu.Lock(); defer c.mu.Unlock(); return c.conn }, func() error { return fn(ic) }))
}

func (c *client) Select(ctx context.Context, folder string) (string, error) {
	var v string
	err := c.call(ctx, func(ic *imapclient.Client) error {
		d, err := ic.Select(folder, &imap.SelectOptions{ReadOnly: false}).Wait()
		if err != nil {
			return fmt.Errorf("imap: select: %w", err)
		}
		v = strconv.FormatUint(uint64(d.UIDValidity), 10)
		return nil
	})
	return v, err
}

func (c *client) SearchUnseen(ctx context.Context, since time.Time) ([]string, error) {
	var out []string
	err := c.call(ctx, func(ic *imapclient.Client) error {
		d, err := ic.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}, Since: since}, nil).Wait()
		if err != nil {
			return fmt.Errorf("imap: search: %w", err)
		}
		for _, u := range d.AllUIDs() {
			out = append(out, strconv.FormatUint(uint64(u), 10))
		}
		return nil
	})
	return out, err
}

func parseUID(uid string) (imap.UID, error) {
	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("imap: invalid uid %q", uid)
	}
	return imap.UID(n), nil
}

// Fetch returns the raw message, or nil when it is missing or larger than
// MaxFetchBytes. It peeks (BODY.PEEK[]) so a failed processing pass leaves the
// message unseen for the next poll; MarkSeen is the only thing that sets \Seen.
func (c *client) Fetch(ctx context.Context, uid string) ([]byte, error) {
	u, err := parseUID(uid)
	if err != nil {
		return nil, err
	}
	var out []byte
	err = c.call(ctx, func(ic *imapclient.Client) error {
		sec := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: MaxFetchBytes + 1}}
		msgs, err := ic.Fetch(imap.UIDSetNum(u), &imap.FetchOptions{UID: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{sec}}).Collect()
		if err != nil {
			return fmt.Errorf("imap: fetch: %w", err)
		}
		if len(msgs) == 0 {
			return nil
		}
		m := msgs[0]
		if m.RFC822Size > MaxFetchBytes {
			return nil
		}
		b := m.FindBodySection(sec)
		if len(b) > MaxFetchBytes {
			return nil
		}
		out = b
		return nil
	})
	return out, err
}

func (c *client) MarkSeen(ctx context.Context, uid string) error {
	u, err := parseUID(uid)
	if err != nil {
		return err
	}
	return c.call(ctx, func(ic *imapclient.Client) error {
		if err := ic.Store(imap.UIDSetNum(u), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); err != nil {
			return fmt.Errorf("imap: store: %w", err)
		}
		return nil
	})
}

// Logout closes the connection; it never blocks on a dead server.
func (c *client) Logout() {
	c.mu.Lock()
	ic, nc := c.c, c.conn
	c.c, c.conn = nil, nil
	c.mu.Unlock()
	if ic != nil {
		done := make(chan struct{})
		go func() { ic.Logout().Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		ic.Close()
	}
	if nc != nil {
		nc.Close()
	}
}
