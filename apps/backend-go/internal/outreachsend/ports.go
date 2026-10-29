// Package outreachsend holds the send pipeline and the ports its consumers (routes,
// worker handler, MCP adapter) depend on.
package outreachsend

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Reservation is the result of a caps reservation. OK=false means refused. Ref is
// the implementation's opaque handle, handed back to Settle/Release.
type Reservation struct {
	OK  bool
	Ref any
}

// Caps meters paid sends (plan 08 autocaps). A nil Caps is valid only when
// OUTREACH_SEND_COST_USD is 0; with a positive cost and no Caps the worker hands the
// job back to Python (D7), never sending unmetered. Calls run inside the caller's tx.
type Caps interface {
	TryReserve(ctx context.Context, tx pgx.Tx, workspaceID, triggerID string, costUSD float64, idem string) (Reservation, error)
	Settle(ctx context.Context, tx pgx.Tx, r Reservation) error
	Release(ctx context.Context, tx pgx.Tx, r Reservation) error
}

// MailConfig is the SMTP connection a Mailer dials. Password is never logged.
// Task 11 (internal/smtpclient) may alias this as smtpclient.Config.
type MailConfig struct {
	Host     string
	Port     int
	Email    string
	Password string
	UseTLS   bool
}

// Mailer sends one RFC 5322 message and returns the Message-ID. It exists so the
// send handler is testable without a socket.
type Mailer interface {
	Send(ctx context.Context, cfg MailConfig, from, to string, msg []byte) (messageID string, err error)
}
