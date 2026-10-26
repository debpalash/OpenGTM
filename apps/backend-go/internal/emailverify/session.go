package emailverify

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// SMTPSession is one SMTP conversation, the seam the probe is written against
// (smtplib.SMTP in Python). The production implementation is NewNetSession;
// tests inject a scripted one. A session only ever speaks EHLO/HELO, MAIL FROM,
// RCPT TO and QUIT: there is deliberately no DATA, AUTH, STARTTLS or VRFY.
//
// Errors: ErrDisconnected = the server went away (smtplib.SMTPServerDisconnected);
// ErrSMTP = a protocol-level failure for this command (smtplib.SMTPException);
// anything else is a transport failure (OSError) that aborts the probe.
type SMTPSession interface {
	Connect(ctx context.Context, host string, port int) error
	EhloOrHelo(name string) error
	Helo(name string) error
	Mail(from string) (code int, err error)
	Rcpt(addr string) (code int, err error)
	Quit() error
	Close()
}

var (
	// ErrDisconnected is smtplib.SMTPServerDisconnected.
	ErrDisconnected = errors.New("smtp: server disconnected")
	// ErrSMTP is smtplib.SMTPException (a refused or malformed command).
	ErrSMTP = errors.New("smtp: protocol error")
)

// maxReplyBytes bounds everything read from one server.
const maxReplyBytes = 256 << 10

// DialFunc is the egress-guarded dial (egress.NewDialContext).
type DialFunc = egress.DialContext

type netSession struct {
	dial   DialFunc
	budget time.Duration

	mu   sync.Mutex
	conn net.Conn
	r    *textproto.Reader
	done chan struct{}
}

// NewNetSession returns a real SMTP session whose whole conversation, dial
// included, must finish within budget. dial must be the egress guard, so a
// private or rebinding MX host is refused before any byte is sent.
func NewNetSession(dial DialFunc, budget time.Duration) SMTPSession {
	if budget <= 0 {
		budget = DefaultSMTPBudget
	}
	return &netSession{dial: dial, budget: budget}
}

func (s *netSession) Connect(ctx context.Context, host string, port int) error {
	ctx, cancel := context.WithTimeout(ctx, s.budget)
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]") // IPv6 literal from DNS data
	conn, err := s.dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		cancel()
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(s.budget))
	done := make(chan struct{})
	s.mu.Lock()
	s.conn, s.done = conn, done
	s.r = textproto.NewReader(bufio.NewReader(io.LimitReader(conn, maxReplyBytes)))
	s.mu.Unlock()
	go func() { // abort a blocked read as soon as the caller's context ends
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
		cancel()
	}()
	code, err := s.reply()
	if err != nil {
		return err
	}
	if code != 220 {
		return fmt.Errorf("%w: greeting %d", ErrSMTP, code)
	}
	return nil
}

// reply reads one (possibly multi-line) response.
func (s *netSession) reply() (int, error) {
	if s.r == nil {
		return 0, ErrDisconnected
	}
	code, _, err := s.r.ReadResponse(0)
	if err == nil {
		return code, nil
	}
	var perr textproto.ProtocolError
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		return 0, ErrDisconnected
	case errors.As(err, &perr):
		return 0, fmt.Errorf("%w: %v", ErrSMTP, err)
	}
	return 0, err
}

func (s *netSession) send(line string) error {
	if s.conn == nil {
		return ErrDisconnected
	}
	if _, err := io.WriteString(s.conn, line+"\r\n"); err != nil {
		return err
	}
	return nil
}

func (s *netSession) cmd(line string) (int, error) {
	if err := s.send(line); err != nil {
		return 0, err
	}
	return s.reply()
}

// safeToken reports whether v can sit inside a command line: printable ASCII
// without space, angle brackets or CR/LF. Anything else (an injected "\r\nDATA",
// a non-ASCII address smtplib would refuse without SMTPUTF8) is refused locally
// and never reaches the wire.
func safeToken(v string) bool {
	if v == "" || len(v) > 254 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c <= 0x20 || c >= 0x7f || c == '<' || c == '>' {
			return false
		}
	}
	return true
}

func (s *netSession) EhloOrHelo(name string) error {
	if !safeToken(name) {
		return fmt.Errorf("%w: bad hello name", ErrSMTP)
	}
	code, err := s.cmd("EHLO " + name)
	if err != nil {
		return err
	}
	if code == 250 {
		return nil
	}
	if code, err = s.cmd("HELO " + name); err != nil {
		return err
	}
	if code != 250 {
		return fmt.Errorf("%w: HELO %d", ErrSMTP, code)
	}
	return nil
}

func (s *netSession) Helo(name string) error {
	if !safeToken(name) {
		return fmt.Errorf("%w: bad hello name", ErrSMTP)
	}
	code, err := s.cmd("HELO " + name)
	if err != nil {
		return err
	}
	if code != 250 {
		return fmt.Errorf("%w: HELO %d", ErrSMTP, code)
	}
	return nil
}

func (s *netSession) Mail(from string) (int, error) {
	if !safeToken(from) {
		return 0, fmt.Errorf("%w: bad sender", ErrSMTP)
	}
	return s.cmd("MAIL FROM:<" + from + ">")
}

func (s *netSession) Rcpt(addr string) (int, error) {
	if !safeToken(addr) {
		return 0, fmt.Errorf("%w: bad recipient", ErrSMTP)
	}
	return s.cmd("RCPT TO:<" + addr + ">")
}

func (s *netSession) Quit() error {
	if s.conn == nil {
		return ErrDisconnected
	}
	_, err := s.cmd("QUIT")
	s.Close()
	return err
}

func (s *netSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
}
