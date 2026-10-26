package emailverify

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// smtpServer is an in-process SMTP server. It records every command line and
// answers RCPT 550 for addresses starting with "reject" and 250 otherwise
// ("random" probes are accepted only when catchAll is set).
type smtpServer struct {
	ln       net.Listener
	catchAll bool
	hang     bool // accept, send the greeting, then never answer (budget tests)

	mu       sync.Mutex
	cmds     []string
	accepted int
}

func newSMTPServer(t *testing.T, catchAll bool) *smtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpServer{ln: ln, catchAll: catchAll}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *smtpServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *smtpServer) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func (s *smtpServer) acceptedConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

func (s *smtpServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.accepted++
		s.mu.Unlock()
		go s.handle(c)
	}
}

func (s *smtpServer) handle(c net.Conn) {
	defer c.Close()
	tp := textproto.NewConn(c)
	_ = tp.PrintfLine("220 test ESMTP")
	if s.hang {
		time.Sleep(30 * time.Second)
		return
	}
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.cmds = append(s.cmds, line)
		s.mu.Unlock()
		verb := strings.ToUpper(strings.Fields(line + " ")[0])
		switch verb {
		case "EHLO", "HELO":
			_ = tp.PrintfLine("250 hello")
		case "MAIL":
			_ = tp.PrintfLine("250 ok")
		case "RCPT":
			addr := strings.ToLower(line)
			switch {
			case strings.Contains(addr, "reject"):
				_ = tp.PrintfLine("550 no such user")
			case strings.Contains(addr, "abcdefghijklmnop") && !s.catchAll:
				_ = tp.PrintfLine("550 no such user")
			default:
				_ = tp.PrintfLine("250 ok")
			}
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default: // DATA, AUTH, VRFY, ... are never expected
			_ = tp.PrintfLine("502 not implemented")
		}
	}
}

func liveDeps(t *testing.T, appEnv string, mx map[string]string, port int) Deps {
	t.Helper()
	d := NewDeps()
	d.Resolver = resolverFunc(func(_ context.Context, domain string) ([]*net.MX, error) {
		h, ok := mx[domain]
		if !ok {
			return nil, errors.New("NXDOMAIN")
		}
		return []*net.MX{{Pref: 10, Host: h + "."}}, nil
	})
	dial := egress.NewDialContext(appEnv, []string{"127.0.0.1"})
	d.NewSession = func() SMTPSession { return NewNetSession(dial, d.Timeout()) }
	d.SMTPPort = port
	d.RandLocal = func() string { return "abcdefghijklmnop" }
	return d
}

type resolverFunc func(ctx context.Context, domain string) ([]*net.MX, error)

func (f resolverFunc) LookupMX(ctx context.Context, domain string) ([]*net.MX, error) {
	return f(ctx, domain)
}

func TestSMTPProbeNeverSendsData(t *testing.T) {
	srv := newSMTPServer(t, false)
	d := liveDeps(t, "test", map[string]string{"acme.test": "127.0.0.1"}, srv.port())
	cands := []string{"ok@acme.test", "reject@acme.test", "evil@acme.test\r\nDATA", "bad@acme.test>\r\nAUTH PLAIN x", "sp ace@acme.test"}
	p, err := ProbeDomain(context.Background(), d, "acme.test", cands)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Reachable || p.CatchAll {
		t.Fatalf("probe: %+v", p)
	}
	if r := p.Results["ok@acme.test"]; r == nil || !*r {
		t.Errorf("ok: %v", r)
	}
	if r := p.Results["reject@acme.test"]; r == nil || *r {
		t.Errorf("reject: %v", r)
	}
	for _, bad := range cands[2:] {
		if r := p.Results[bad]; r != nil {
			t.Errorf("injection candidate %q must stay unknown, got %v", bad, *r)
		}
	}
	allowed := map[string]bool{"EHLO": true, "HELO": true, "MAIL": true, "RCPT": true, "QUIT": true}
	sawRcpt := 0
	for _, c := range srv.commands() {
		verb := strings.ToUpper(strings.Fields(c)[0])
		if !allowed[verb] {
			t.Errorf("unexpected command %q (never DATA/AUTH/VRFY/STARTTLS)", c)
		}
		if verb == "RCPT" {
			sawRcpt++
		}
		if strings.Contains(strings.ToUpper(c), "AUTH") || strings.Contains(c, "\r") || strings.Contains(c, "\n") {
			t.Errorf("command carries injected content: %q", c)
		}
	}
	if sawRcpt != 3 { // random probe + ok + reject
		t.Errorf("RCPT count = %d, want 3", sawRcpt)
	}
	cmds := srv.commands()
	if last := cmds[len(cmds)-1]; strings.ToUpper(last) != "QUIT" {
		t.Errorf("conversation must end with QUIT, ended with %q", last)
	}
}

func TestCatchAllDetection(t *testing.T) {
	srv := newSMTPServer(t, true)
	d := liveDeps(t, "test", map[string]string{"ca.test": "127.0.0.1"}, srv.port())
	p, err := ProbeDomain(context.Background(), d, "ca.test", []string{"a@ca.test", "reject@ca.test"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Reachable || !p.CatchAll {
		t.Fatalf("expected reachable catch-all, got %+v", p)
	}
	for k, v := range p.Results {
		if v != nil {
			t.Errorf("%s: catch-all domains leave individual results unknown, got %v", k, *v)
		}
	}
	rcpts := 0
	for _, c := range srv.commands() {
		if strings.HasPrefix(strings.ToUpper(c), "RCPT") {
			rcpts++
		}
	}
	if rcpts != 1 {
		t.Errorf("catch-all must stop after the random probe, saw %d RCPT", rcpts)
	}
	// ProbeSMTP shares the catch-all cache: second probe sends no random RCPT.
	before := len(srv.commands())
	pr, err := ProbeSMTP(context.Background(), d, "127.0.0.1", "again@ca.test")
	if err != nil {
		t.Fatal(err)
	}
	if !pr.CatchAll {
		t.Fatal("ProbeSMTP should reuse the cached catch-all verdict")
	}
	for _, c := range srv.commands()[before:] {
		if strings.HasPrefix(strings.ToUpper(c), "RCPT") {
			t.Errorf("unexpected RCPT after cached catch-all: %q", c)
		}
	}
}

func TestSMTPProbeRefusesPrivateMX(t *testing.T) {
	srv := newSMTPServer(t, false)
	for _, host := range []string{"127.0.0.1", "localhost", "10.1.2.3", "169.254.169.254", "::1", "[::ffff:127.0.0.1]", "2130706433", "0x7f000001"} {
		t.Run(host, func(t *testing.T) {
			d := liveDeps(t, "production", map[string]string{"evil.test": host}, srv.port())
			// production env: the test allowlist is ignored, loopback must be refused at dial time
			p, err := ProbeDomain(context.Background(), d, "evil.test", []string{"a@evil.test"})
			if err != nil {
				t.Fatalf("a refused MX is an unknown result, not an error: %v", err)
			}
			if p.Reachable || p.Results["a@evil.test"] != nil {
				t.Fatalf("probe must be unreachable/unknown: %+v", p)
			}
			_, err = ProbeSMTP(context.Background(), d, host, "a@evil.test")
			if !errors.Is(err, egress.ErrBlocked) {
				t.Fatalf("ProbeSMTP(%s) err = %v, want egress.ErrBlocked", host, err)
			}
		})
	}
	if n := srv.acceptedConns(); n != 0 {
		t.Fatalf("server accepted %d connection(s); a private MX must never be dialled", n)
	}
}

func TestSMTPProbeBudgetAndCancel(t *testing.T) {
	srv := newSMTPServer(t, false)
	srv.hang = true
	d := liveDeps(t, "test", map[string]string{"slow.test": "127.0.0.1"}, srv.port())
	d.SMTPBudget = 300 * time.Millisecond
	start := time.Now()
	p, err := ProbeDomain(context.Background(), d, "slow.test", []string{"a@slow.test"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Reachable || p.Results["a@slow.test"] != nil {
		t.Fatalf("hung server must yield unknown: %+v", p)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("probe ignored its budget, took %s", el)
	}
	// context cancellation aborts a hung conversation promptly and reports the cancel
	d.SMTPBudget = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = ProbeDomain(ctx, d, "slow.test", []string{"a@slow.test"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("cancel not honoured, took %s", el)
	}
}

var _ = bufio.NewReader
var _ = strconv.Itoa
