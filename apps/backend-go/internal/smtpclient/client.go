// Package smtpclient sends one message over SMTP with the protocol the Python
// sender (sender.send_email) uses, hardened for customer-configured hosts: the
// caller supplies a guarded dial (egress.NewDialContext), certificates are
// always verified, and the password is never sent in clear except to an
// allow-listed test host. Errors carry the server reply verbatim ("550 5.1.1
// ...") so IsHardBounce sees what Python's str(exc) showed it.
package smtpclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds the whole exchange when Client.Timeout is zero.
const DefaultTimeout = 60 * time.Second

// Config is one workspace's SMTP settings plus the dial function to use.
type Config struct {
	Host     string
	Port     int
	Email    string
	Password string
	UseTLS   bool
	// Dial must be an SSRF-guarded dialer (egress.NewDialContext). Send refuses
	// to run without one rather than fall back to an unguarded net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Client sends messages. The zero value is usable.
type Client struct {
	// Timeout bounds dial, TLS, AUTH and the transaction together (default 60s).
	Timeout time.Duration
	// RootCAs overrides the system roots (tests only).
	RootCAs *x509.CertPool
	// PlainHosts are allow-listed test hosts that may be used without TLS.
	PlainHosts []string
}

// Send delivers msg (full RFC 5322 text, CRLF line endings) from -> to and
// returns the message's Message-ID header value ("" if it has none).
//
// Protocol, matching Python where Go hardens it (see D2):
//   - UseTLS and port 587: STARTTLS, required.
//   - UseTLS and port 465: implicit TLS (Python's smtplib fallback would have
//     failed here; documented deviation).
//   - UseTLS on any other port: STARTTLS when offered.
//   - everything else is plaintext, refused unless Host is in PlainHosts.
func (c *Client) Send(ctx context.Context, cfg Config, from, to string, msg []byte) (string, error) {
	id, err := c.send(ctx, cfg, from, to, msg)
	if err != nil {
		return "", redact(err, cfg.Password)
	}
	return id, nil
}

func (c *Client) plainAllowed(host string) bool {
	for _, h := range c.PlainHosts {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}

var errPlaintext = errors.New("smtp: plaintext authentication refused (TLS required for this host)")

func (c *Client) send(ctx context.Context, cfg Config, from, to string, msg []byte) (string, error) {
	if cfg.Dial == nil {
		return "", errors.New("smtp: no guarded dialer configured")
	}
	if !cfg.UseTLS && !c.plainAllowed(cfg.Host) {
		return "", errPlaintext
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12, RootCAs: c.RootCAs}
	raw, err := cfg.Dial(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return "", wrapCtx(ctx, err)
	}
	// Cancellation and timeout close the socket so every blocked read returns.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-done:
		}
	}()
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	defer raw.Close()

	var conn net.Conn = raw
	if cfg.UseTLS && cfg.Port == 465 {
		tc := tls.Client(raw, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return "", wrapCtx(ctx, err)
		}
		conn = tc
	}
	cl, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return "", wrapCtx(ctx, err)
	}
	if err := c.session(ctx, cl, cfg, tlsCfg, from, to, msg); err != nil {
		return "", wrapCtx(ctx, err)
	}
	return messageID(msg), nil
}

func (c *Client) session(ctx context.Context, cl *smtp.Client, cfg Config, tlsCfg *tls.Config, from, to string, msg []byte) error {
	if err := cl.Hello("localhost"); err != nil {
		return err
	}
	secure := cfg.UseTLS && cfg.Port == 465
	if cfg.UseTLS && cfg.Port != 465 {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(tlsCfg); err != nil {
				return err
			}
			secure = true
		} else if cfg.Port == 587 {
			return errors.New("smtp: server does not offer STARTTLS on port 587")
		}
	}
	if !secure && !c.plainAllowed(cfg.Host) {
		return errPlaintext
	}
	ok, mechs := cl.Extension("AUTH")
	if !ok {
		return errors.New("smtp: server does not support AUTH")
	}
	var auth smtp.Auth = &loginAuth{user: cfg.Email, pass: cfg.Password}
	if hasMech(mechs, "PLAIN") {
		auth = &plainAuth{user: cfg.Email, pass: cfg.Password}
	} else if !hasMech(mechs, "LOGIN") {
		return errors.New("smtp: no supported AUTH mechanism (PLAIN, LOGIN)")
	}
	if err := cl.Auth(auth); err != nil {
		return err
	}
	if err := cl.Mail(from); err != nil {
		return err
	}
	if err := cl.Rcpt(to); err != nil {
		return err
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	_ = cl.Quit() // the message is accepted; a QUIT failure changes nothing
	return nil
}

func hasMech(list, want string) bool {
	for _, m := range strings.Fields(list) {
		if strings.EqualFold(m, want) {
			return true
		}
	}
	return false
}

// plainAuth is AUTH PLAIN without net/smtp's refusal on non-TLS links (the
// caller has already decided plaintext is acceptable for this host).
type plainAuth struct{ user, pass string }

func (a *plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}
func (a *plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("smtp: unexpected PLAIN challenge")
	}
	return nil, nil
}

type loginAuth struct{ user, pass string }

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }
func (a *loginAuth) Next(prompt []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(prompt))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, errors.New("smtp: unexpected LOGIN challenge")
}

// messageID returns the Message-ID header value of msg's header block.
func messageID(msg []byte) string {
	sc := bufio.NewScanner(strings.NewReader(string(msg)))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var cur string
	found := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && found {
			cur += " " + strings.TrimSpace(line)
			continue
		}
		if found {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "message-id") {
			cur, found = strings.TrimSpace(v), true
		}
	}
	return cur
}

// wrapCtx reports a cancelled or timed-out exchange as such (the socket close
// the watcher performs surfaces as an opaque read error otherwise).
func wrapCtx(ctx context.Context, err error) error {
	// net/smtp renders replies as `550 "5.1.1 ..."` (%q); the classifier and
	// operators want the reply text verbatim: `550 5.1.1 ...`.
	var te *textproto.Error
	if errors.As(err, &te) {
		err = &wrapped{msg: fmt.Sprintf("%d %s", te.Code, te.Msg), err: err}
	}
	if ce := ctx.Err(); ce != nil && !errors.Is(err, ce) {
		return &wrapped{msg: fmt.Sprintf("smtp: %s: %v", ce, err), err: ce}
	}
	return err
}

type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg }
func (w *wrapped) Unwrap() error { return w.err }

// redactedError scrubs the password (and its base64 form) from the text while
// keeping the original reachable through errors.Is/As.
type redactedError struct {
	msg string
	err error
}

func (r *redactedError) Error() string { return r.msg }
func (r *redactedError) Unwrap() error { return r.err }

func redact(err error, password string) error {
	if password == "" {
		return err
	}
	msg := err.Error()
	scrubbed := strings.ReplaceAll(msg, password, "[redacted]")
	scrubbed = strings.ReplaceAll(scrubbed, base64.StdEncoding.EncodeToString([]byte(password)), "[redacted]")
	if scrubbed == msg {
		return err
	}
	return &redactedError{msg: scrubbed, err: err}
}
