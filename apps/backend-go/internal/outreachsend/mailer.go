package outreachsend

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autocaps"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/smtpclient"
	"github.com/jackc/pgx/v5"
)

// SMTPMailer is the production Mailer: smtpclient over the SSRF-guarded dial.
// Client carries no PlainHosts and no RootCAs (those are test-only knobs).
type SMTPMailer struct {
	Client *smtpclient.Client
	Dial   egress.DialContext
}

// NewSMTPMailer wires the production mailer for appEnv (egress allow-list empty).
func NewSMTPMailer(appEnv string) *SMTPMailer {
	return &SMTPMailer{Client: &smtpclient.Client{}, Dial: egress.NewDialContext(appEnv, nil)}
}

// Send implements Mailer.
func (m *SMTPMailer) Send(ctx context.Context, cfg MailConfig, from, to string, msg []byte) (string, error) {
	return m.Client.Send(ctx, smtpclient.Config{
		Host: cfg.Host, Port: cfg.Port, Email: cfg.Email, Password: cfg.Password, UseTLS: cfg.UseTLS, Dial: m.Dial,
	}, from, to, msg)
}

// AutoCaps adapts plan 08 autocaps to the Caps port. Load, when set, loads the
// per-rule limits of triggerID; nil means unlimited per rule (Python passes a
// stand-in trigger with no limits for outreach). GlobalDailyUSD is
// AUTOMATIONS_GLOBAL_DAILY_USD.
type AutoCaps struct {
	GlobalDailyUSD float64
	Load           func(ctx context.Context, tx pgx.Tx, triggerID string) (autocaps.TriggerCaps, error)
}

// TryReserve implements Caps.
func (a AutoCaps) TryReserve(ctx context.Context, tx pgx.Tx, ws, triggerID string, cost float64, idem string) (Reservation, error) {
	tc := autocaps.TriggerCaps{}
	if a.Load != nil {
		var err error
		if tc, err = a.Load(ctx, tx, triggerID); err != nil {
			return Reservation{}, err
		}
	}
	tc.ID = triggerID
	r, err := autocaps.TryReserve(ctx, tx, tc, ws, cost, idem, a.GlobalDailyUSD)
	if err != nil {
		return Reservation{}, err
	}
	return Reservation{OK: r.OK, Ref: r}, nil
}

// Settle implements Caps.
func (AutoCaps) Settle(ctx context.Context, tx pgx.Tx, r Reservation) error {
	if v, ok := r.Ref.(autocaps.Reservation); ok {
		return autocaps.Settle(ctx, tx, v)
	}
	return nil
}

// Release implements Caps.
func (AutoCaps) Release(ctx context.Context, tx pgx.Tx, r Reservation) error {
	if v, ok := r.Ref.(autocaps.Reservation); ok {
		return autocaps.Release(ctx, tx, v)
	}
	return nil
}
