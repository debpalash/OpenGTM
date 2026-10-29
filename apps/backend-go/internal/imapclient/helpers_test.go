package imapclient

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const (
	testUser     = "bounces@sender.example"
	testPassword = "imap-s3cret-DO-NOT-LEAK"
)

type mode int

const (
	modeImplicitTLS mode = iota
	modeSTARTTLS
	modePlain
)

type testServer struct {
	addr    string
	pool    *x509.CertPool
	user    *imapmemserver.User
	accepts atomic.Int32
}

func selfSigned(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// newServer starts an in-memory IMAP server with one user and an INBOX.
func newServer(t testing.TB, m mode) *testServer {
	t.Helper()
	cert, pool := selfSigned(t)
	mem := imapmemserver.New()
	u := imapmemserver.NewUser(testUser, testPassword)
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(u)
	opts := &imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
	}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	if m == modePlain {
		opts.InsecureAuth = true
	} else if m == modeSTARTTLS {
		opts.TLSConfig = tlsCfg
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{addr: ln.Addr().String(), pool: pool, user: u}
	var l net.Listener = &countListener{Listener: ln, n: &ts.accepts}
	if m == modeImplicitTLS {
		l = tls.NewListener(l, tlsCfg)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return ts
}

type countListener struct {
	net.Listener
	n *atomic.Int32
}

func (c *countListener) Accept() (net.Conn, error) {
	conn, err := c.Listener.Accept()
	if err == nil {
		c.n.Add(1)
	}
	return conn, err
}

func (ts *testServer) add(t testing.TB, body string, when time.Time, flags ...imap.Flag) {
	t.Helper()
	lit := &literal{strings.NewReader(body), int64(len(body))}
	_, err := ts.user.Append("INBOX", lit, &imap.AppendOptions{Flags: flags, Time: when})
	if err != nil {
		t.Fatal(err)
	}
}

type literal struct {
	*strings.Reader
	n int64
}

func (l *literal) Size() int64 { return l.n }

// rawServer is a scripted TLS IMAP-ish server: greeting, then handler per line.
func rawServer(t testing.TB, handle func(tag, cmd string) string) (addr string, pool *x509.CertPool) {
	t.Helper()
	cert, pool := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write([]byte("* OK ready\r\n"))
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					tag, cmd, _ := strings.Cut(strings.TrimSpace(line), " ")
					c.Write([]byte(handle(tag, cmd)))
				}
			}()
		}
	}()
	return ln.Addr().String(), pool
}
