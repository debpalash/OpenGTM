// Package testsmtp is an in-process SMTP server for tests: STARTTLS (and
// optionally implicit TLS) with a throwaway self-signed certificate, AUTH
// PLAIN/LOGIN, scripted per-recipient replies, and a record of every message.
// Nothing here touches the network beyond a loopback listener.
package testsmtp

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options script the server. The zero value is a plain STARTTLS-capable server
// that accepts any credentials and any recipient.
type Options struct {
	// ImplicitTLS wraps every connection in TLS at accept (port 465 style).
	ImplicitTLS bool
	// NoSTARTTLS stops advertising STARTTLS.
	NoSTARTTLS bool
	// AuthUser/AuthPass, when AuthUser is set, are the only accepted credentials.
	AuthUser, AuthPass string
	// AuthFailReply overrides the AUTH failure line, default "535 5.7.8 authentication failed".
	AuthFailReply string
	// RcptReplies maps a recipient address (lower-case) to the full reply line
	// for RCPT TO, for example "550 5.1.1 <a@b.example>: user unknown".
	RcptReplies map[string]string
	// Hang makes the server accept and then never answer (timeout tests).
	Hang bool
}

// Message is one accepted message.
type Message struct {
	From     string
	To       []string
	Data     []byte // dot-unstuffed, CRLF line endings as received
	TLS      bool   // delivered over TLS
	AuthUser string
}

// Server is a running fake SMTP server.
type Server struct {
	opts Options
	ln   net.Listener
	cert tls.Certificate
	pool *x509.CertPool

	mu       sync.Mutex
	messages []Message
	conns    []net.Conn
	wg       sync.WaitGroup
}

// New starts a server on 127.0.0.1 and registers cleanup with t.
func New(t testing.TB, opts Options) *Server {
	t.Helper()
	cert, pool, err := selfSigned()
	if err != nil {
		t.Fatalf("testsmtp cert: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testsmtp listen: %v", err)
	}
	s := &Server{opts: opts, ln: ln, cert: cert, pool: pool}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.Close)
	return s
}

// Addr is the listener address, host:port.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Host and Port split Addr.
func (s *Server) Host() string { h, _, _ := net.SplitHostPort(s.Addr()); return h }
func (s *Server) Port() int {
	_, p, _ := net.SplitHostPort(s.Addr())
	n, _ := strconv.Atoi(p)
	return n
}

// CertPool trusts the server's self-signed certificate.
func (s *Server) CertPool() *x509.CertPool { return s.pool }

// Messages is a copy of the accepted messages.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

// Close stops the listener and drops open connections.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer c.Close()
			s.serve(c)
		}()
	}
}

func (s *Server) tlsConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{s.cert}, MinVersion: tls.VersionTLS12}
}

func (s *Server) serve(raw net.Conn) {
	if s.opts.Hang { // never greet, never handshake; ends when the peer or Close drops the socket
		_, _ = io.Copy(io.Discard, raw)
		return
	}
	var conn net.Conn = raw
	isTLS := false
	if s.opts.ImplicitTLS {
		conn = tls.Server(raw, s.tlsConfig())
		isTLS = true
	}
	r := bufio.NewReader(conn)
	w := func(format string, a ...any) { fmt.Fprintf(conn, format+"\r\n", a...) }
	w("220 testsmtp ready")
	var from, authUser string
	var rcpts []string
	authed := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			w("250-testsmtp")
			if !s.opts.NoSTARTTLS && !isTLS {
				w("250-STARTTLS")
			}
			w("250-8BITMIME")
			w("250 AUTH PLAIN LOGIN")
		case "HELO":
			w("250 testsmtp")
		case "STARTTLS":
			if s.opts.NoSTARTTLS || isTLS {
				w("502 5.5.1 STARTTLS not available")
				continue
			}
			w("220 2.0.0 ready to start TLS")
			conn = tls.Server(raw, s.tlsConfig())
			r = bufio.NewReader(conn)
			w = func(format string, a ...any) { fmt.Fprintf(conn, format+"\r\n", a...) }
			isTLS = true
			from, rcpts, authed, authUser = "", nil, false, ""
		case "AUTH":
			mech, ir, _ := strings.Cut(arg, " ")
			var user, pass string
			switch strings.ToUpper(mech) {
			case "PLAIN":
				if ir == "" {
					w("334 ")
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					ir = strings.TrimSpace(l)
				}
				b, _ := base64.StdEncoding.DecodeString(ir)
				parts := bytes.Split(b, []byte{0})
				if len(parts) == 3 {
					user, pass = string(parts[1]), string(parts[2])
				}
			case "LOGIN":
				w("334 %s", base64.StdEncoding.EncodeToString([]byte("Username:")))
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				u, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
				w("334 %s", base64.StdEncoding.EncodeToString([]byte("Password:")))
				l, err = r.ReadString('\n')
				if err != nil {
					return
				}
				p, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
				user, pass = string(u), string(p)
			default:
				w("504 5.5.4 unrecognized mechanism")
				continue
			}
			if s.opts.AuthUser != "" && (user != s.opts.AuthUser || pass != s.opts.AuthPass) {
				if s.opts.AuthFailReply != "" {
					w("%s", s.opts.AuthFailReply)
				} else {
					w("535 5.7.8 authentication failed")
				}
				continue
			}
			authed, authUser = true, user
			w("235 2.7.0 authenticated")
		case "MAIL":
			if s.opts.AuthUser != "" && !authed {
				w("530 5.7.0 authentication required")
				continue
			}
			from = between(arg)
			rcpts = nil
			w("250 2.1.0 ok")
		case "RCPT":
			to := between(arg)
			if rep, ok := s.opts.RcptReplies[strings.ToLower(to)]; ok {
				w("%s", rep)
				continue
			}
			rcpts = append(rcpts, to)
			w("250 2.1.5 ok")
		case "DATA":
			if len(rcpts) == 0 {
				w("554 5.5.1 no valid recipients")
				continue
			}
			w("354 end data with <CRLF>.<CRLF>")
			var buf bytes.Buffer
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				buf.WriteString(strings.TrimPrefix(l, ".")) // undo dot stuffing
			}
			s.mu.Lock()
			s.messages = append(s.messages, Message{From: from, To: rcpts, Data: buf.Bytes(), TLS: isTLS, AuthUser: authUser})
			s.mu.Unlock()
			rcpts = nil
			w("250 2.0.0 queued")
		case "RSET":
			from, rcpts = "", nil
			w("250 2.0.0 ok")
		case "NOOP":
			w("250 2.0.0 ok")
		case "QUIT":
			w("221 2.0.0 bye")
			return
		default:
			w("500 5.5.2 command not recognized")
		}
	}
}

func between(arg string) string {
	i, j := strings.Index(arg, "<"), strings.LastIndex(arg, ">")
	if i < 0 || j < i {
		return ""
	}
	return arg[i+1 : j]
}

func selfSigned() (tls.Certificate, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "testsmtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool, nil
}
