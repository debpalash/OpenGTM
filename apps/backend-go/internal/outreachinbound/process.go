// Package outreachinbound is the inbound IMAP poll for async bounces and
// complaints (Python services/outreach/inbound.py): per workspace it fetches
// unseen mail, classifies it with dsnparse, maps it to one of OUR sends by the
// server-generated Message-ID and applies the effect exactly once.
package outreachinbound

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/dsnparse"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
)

// Process is _process_one. It must run inside tenancy.InWorkspace(wsID): the
// ledger insert is the idempotency gate and ApplyBounce runs in the SAME
// transaction, only when the ledger row is new, so a retry, a re-fetch or a
// concurrent poller can neither lose nor double-count a bounce.
//
// Only a hard Message-ID match to one of our own sends is acted on. Anything else
// is recorded as unmatched (records parsed) or unknown (none: an empty, garbled
// or non-ASCII DSN, which Python could only raise on) with the recipient-only
// send recorded for visibility but never acted on.
func Process(ctx context.Context, tx pgx.Tx, wsID, uidValidity, uid string, raw []byte, th outreachstore.Thresholds) error {
	srcMID := stripNUL(dsnparse.SourceMessageID(raw))
	records := dsnparse.Parse(raw)
	for i := range records { // Postgres text rejects 0x00: it would poison every retry
		r := &records[i]
		r.Recipient, r.Diagnostic, r.OriginalMessageID = stripNUL(r.Recipient), stripNUL(r.Diagnostic), stripNUL(r.OriginalMessageID)
	}

	var chosen *dsnparse.Record
	var matched *outreachstore.SendRef
	for i := range records {
		r := &records[i]
		if r.OriginalMessageID == "" {
			continue
		}
		send, err := outreachstore.GetSendByMessageID(ctx, tx, wsID, r.OriginalMessageID)
		if err != nil {
			return fmt.Errorf("outreachinbound: match send: %w", err)
		}
		if send != nil {
			chosen, matched = r, send
			break
		}
	}

	if chosen == nil {
		in := outreachstore.InboundIn{WorkspaceID: wsID, IMAPUID: uid, UIDValidity: uidValidity, SourceMessageID: srcMID,
			Kind: "unknown", Diagnostic: "no bounce record"}
		if len(records) > 0 {
			rec0 := records[0]
			in.Kind, in.Recipient, in.Diagnostic = "unmatched", rec0.Recipient, rec0.Diagnostic
			if rec0.Recipient != "" {
				fb, err := outreachstore.GetRecentSendTo(ctx, tx, wsID, rec0.Recipient)
				if err != nil {
					return fmt.Errorf("outreachinbound: fallback lookup: %w", err)
				}
				if fb != nil {
					id := fb.ID
					in.MatchedSendID = &id
				}
			}
		}
		_, err := outreachstore.RecordInboundProcessed(ctx, tx, in)
		return err
	}

	recipient := chosen.Recipient
	if recipient == "" {
		recipient = matched.ToEmail
	}
	sendID := matched.ID
	created, err := outreachstore.RecordInboundProcessed(ctx, tx, outreachstore.InboundIn{
		WorkspaceID: wsID, IMAPUID: uid, UIDValidity: uidValidity, SourceMessageID: srcMID,
		MatchedSendID: &sendID, Kind: chosen.Kind, Recipient: recipient, Diagnostic: chosen.Diagnostic,
	})
	if err != nil {
		return err
	}
	if !created {
		return nil // already processed
	}
	a := outreachstore.BounceApply{WorkspaceID: wsID, SendID: &sendID, SequenceID: matched.SequenceID,
		ToEmail: matched.ToEmail, Kind: chosen.Kind, Diagnostic: chosen.Diagnostic, Source: "imap"}
	if matched.EnrollmentID != nil {
		e := int(*matched.EnrollmentID)
		a.EnrollmentID = &e
	}
	return outreachstore.ApplyBounce(ctx, tx, a, th)
}

func stripNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }
