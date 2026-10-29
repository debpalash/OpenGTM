package copilot

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachdraft"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// This file ports suggest_outreach and draft_grounded_outreach (plan 15 WP-COPILOT-TOOLS). Both only produce
// text: suggest_outreach returns a template message, draft_grounded_outreach persists a draft row through
// internal/outreachdraft. Neither can send, enqueue or schedule mail.

// icpValueProposition is leadgen/config.py ICP["value_proposition"].
const icpValueProposition = "Yupcha Superadmin Console — Unified multi-database CRM " +
	"for candidate tracking, attendance, client management, " +
	"and automated reporting"

func (t *Turn) suggestOutreach(ctx context.Context, a map[string]any) (string, bool, error) {
	raw, present := a["lead_id"]
	if !present {
		return keyErr("suggest_outreach", "lead_id"), true, nil
	}
	id, ok := asInt64(raw)
	if !ok {
		return errLeadNotFound, true, nil // Python looked a non-integer id up and found nothing
	}
	var res string
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		lead, err := leadByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if lead == nil {
			res = errLeadNotFound
			return nil
		}
		contact := sval(lead.ContactPerson)
		if contact == "" {
			contact = "there"
		}
		spec := sval(lead.Specialization)
		if spec == "" {
			spec = "your industry"
		}
		company := lead.Company
		msg := wbcrud.NewObj()
		channel, present := a["channel"]
		if !present {
			channel = "email"
		}
		if channel == "email" {
			in := ""
			if c := sval(lead.City); c != "" {
				in = " in " + c
			}
			to := sval(lead.Email)
			if to == "" {
				to = "(email not found — enrich this lead first)"
			}
			msg.Set("subject", "Streamline "+company+"'s "+spec+" operations")
			msg.Set("body", "Hi "+contact+",\n\n"+
				"I noticed "+company+" specializes in "+spec+in+". "+
				"Companies like yours often struggle with fragmented candidate tracking, "+
				"attendance management, and client reporting.\n\n"+
				"We built "+icpValueProposition+" — specifically for "+spec+" firms. "+
				"It consolidates everything into one unified console.\n\n"+
				"Would you be open to a quick 15-min walkthrough this week?\n\n"+
				"Best regards")
			msg.Set("channel", "email")
			msg.Set("to", to)
		} else {
			profile := sval(lead.LinkedinURL)
			if profile == "" {
				profile = "(LinkedIn not found — enrich first)"
			}
			msg.Set("body", "Hi "+contact+", I came across "+company+"'s profile and was impressed "+
				"by your work in "+spec+". "+
				"We've been helping similar "+spec+" companies streamline operations "+
				"with "+icpValueProposition+". "+
				"Would love to connect and share how we've helped firms like yours. "+
				"Open to a quick chat?")
			msg.Set("channel", "linkedin")
			msg.Set("profile", profile)
		}
		o := wbcrud.NewObj()
		o.Set("outreach", msg)
		o.Set("lead", company)
		res, err = objJSON(o)
		return err
	})
	return res, err == nil, err
}

func draftRefusal(msg string, flags []any) (string, bool, error) {
	o := wbcrud.NewObj()
	o.Set("ok", false)
	o.Set("persisted", false)
	o.Set("error", msg)
	if flags != nil {
		o.Set("contact_flags", flags)
		o.Set("send_performed", false)
	}
	s, err := objJSON(o)
	return s, err == nil, err
}

func (t *Turn) draftGroundedOutreach(ctx context.Context, a map[string]any) (string, bool, error) {
	convID := stripOr(a["conversation_id"], "")
	sourceActionID := stripOr(a["source_action_id"], "")
	found, history, err := t.conversation(ctx, convID)
	if err != nil {
		return "", false, err
	}
	if !found {
		return draftRefusal("Conversation not found", nil)
	}
	source, ok := byActionOrdered(history, "enrich_people_contacts", sourceActionID)
	if !ok {
		return draftRefusal("Saved contact enrichment result not found", nil)
	}
	allowRisky := false
	if v, present := a["allow_risky"]; present {
		allowRisky = pyTruthy(v)
	}
	var out string
	err = t.inTx(ctx, func(tx pgx.Tx) error {
		suppressed := map[string]bool{}
		for _, e := range outreachdraft.Emails(source) {
			s, err := outreachstore.IsSuppressed(ctx, tx, t.WS, e)
			if err != nil {
				return err
			}
			suppressed[e] = s
		}
		rec, err := outreachdraft.Create(ctx, tx, outreachdraft.Request{
			WorkspaceID: t.WS, ConversationID: convID, SourceActionID: sourceActionID,
			RequestedPersonID: strOr(a["person_id"], ""), AllowRisky: allowRisky,
			IdempotencyKey: strOr(a["idempotency_key"], ""), Source: source,
			Suppressed: func(e string) bool { return suppressed[e] }, NewID: t.draftID(),
		})
		var derr *outreachdraft.Error
		if errors.As(err, &derr) {
			out, _, err = draftRefusal(derr.Msg, derr.Flags)
			return err
		}
		if err != nil {
			return err
		}
		out, err = objJSON(rec)
		return err
	})
	return out, err == nil, err
}

// draftID is the draft id source: Engine.NewDraftID in tests, else uuid4.
func (t *Turn) draftID() func() string {
	if t.Engine != nil && t.Engine.NewDraftID != nil {
		return t.Engine.NewDraftID
	}
	return nil
}
