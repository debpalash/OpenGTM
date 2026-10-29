package outreachsend

import (
	"context"
	"testing"
)

// Compile-time shape checks: a nil Caps is valid for free sends and the Mailer
// seam is implementable without a socket.
func TestPortsShape(t *testing.T) {
	var c Caps
	if c != nil {
		t.Fatal("nil Caps must be representable")
	}
	var _ Mailer = (*fakeMailer)(nil)
	if (Reservation{}).OK {
		t.Fatal("zero Reservation is not OK")
	}
}

type fakeMailer struct{}

func (*fakeMailer) Send(_ context.Context, _ MailConfig, _, _ string, _ []byte) (string, error) {
	return "", nil
}
