package mcp

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

const (
	maxEnrollLeadIDs   = 1000
	maxSubjectChars    = 998
	maxBodyHTMLBytes   = 200 * 1024
	enrollConsentLabel = "mcp_enroll"
)

// sendLeadFields are the lead columns handed to the send action, in Python order.
var sendLeadFields = []string{"email", "company", "contact_person", "contact_title", "city", "website", "phone"}

// outreachWriteTools returns enroll_leads and send_email. Both run against
// Deps.Outreach (plan 08, D9); with no adapter they answer
// "<tool> is not available in this build".
func outreachWriteTools(d Deps) []WriteTool {
	return []WriteTool{
		{Name: "enroll_leads", Capability: CapSequencesEnroll, Roles: WriteRoles,
			Apply: func(ctx context.Context, tx pgx.Tx, c Ctx, a Args) (string, error) {
				return enrollLeads(ctx, tx, d, c, a)
			}},
		{Name: "send_email", Capability: CapOutreachSend, Roles: AdminRoles,
			Gate: func(cfg Config) string {
				if !cfg.AutomationsAllowLegacyOutreach {
					return "legacy_outreach_disabled"
				}
				return ""
			},
			Apply: func(ctx context.Context, tx pgx.Tx, c Ctx, a Args) (string, error) {
				return sendEmail(ctx, tx, d, c, a)
			}},
	}
}

func unavailableJSON(tool string) string { return errJSON(tool + " is not available in this build") }

// outreachErr maps a port error: unavailable becomes the documented answer,
// anything else is returned for the pipeline to mask.
func outreachErr(tool string, err error) (string, error) {
	if IsUnavailable(err) {
		return unavailableJSON(tool), nil
	}
	return "", err
}

// normalizeEmail mirrors outreach.normalize.normalize_email (NFKC, trim, lower,
// IDNA domain) and returns "" for an empty address.
func normalizeEmail(email string) string {
	if email == "" {
		return ""
	}
	e := strings.ToLower(strings.TrimSpace(norm.NFKC.String(email)))
	i := strings.LastIndex(e, "@")
	if i < 0 {
		return e
	}
	local, domain := e[:i], e[i+1:]
	if local == "" || domain == "" {
		return e
	}
	if ascii, err := idna.Lookup.ToASCII(domain); err == nil {
		domain = strings.ToLower(ascii)
	}
	return local + "@" + domain
}

func enrollLeads(ctx context.Context, tx pgx.Tx, d Deps, c Ctx, a Args) (string, error) {
	const tool = "enroll_leads"
	if d.Outreach == nil {
		return unavailableJSON(tool), nil
	}
	seqID, _, err := a.String("sequence_id")
	if err != nil {
		return errJSON(err.Error()), nil
	}
	ids, _, err := a.IntList("lead_ids")
	if err != nil {
		return errJSON(err.Error()), nil
	}
	if len(ids) > maxEnrollLeadIDs {
		return errJSON("lead_ids must contain at most 1000 ids"), nil
	}
	op := d.OutreachPort()
	// Anti-orphan / cross-tenant: the sequence must exist in THIS workspace.
	exists := false
	if seqID != "" {
		if exists, err = op.SequenceExists(ctx, tx, c.WorkspaceID, seqID); err != nil {
			return outreachErr(tool, err)
		}
	}
	if !exists {
		return errJSON("Sequence not found"), nil
	}
	enrolled := int64(0)
	skipped := []any{}
	skip := func(id int64, reason string) {
		skipped = append(skipped, pyjson.NewObj("lead_id", id, "reason", reason))
	}
	for _, id := range ids {
		lead, err := d.LeadPort().ByID(ctx, tx, id) // scoped: another workspace's lead is a miss
		if err != nil {
			return outreachErr(tool, err)
		}
		email := ""
		if lead != nil && lead.Email != nil {
			email = normalizeEmail(*lead.Email)
		}
		if email == "" {
			skip(id, "no_email")
			continue
		}
		sup, err := op.IsSuppressed(ctx, tx, c.WorkspaceID, email)
		if err != nil {
			return outreachErr(tool, err)
		}
		if sup {
			skip(id, "suppressed")
			continue
		}
		ok, err := op.Enroll(ctx, tx, c.WorkspaceID, seqID, id, email, enrollConsentLabel)
		if err != nil {
			return outreachErr(tool, err)
		}
		if ok {
			enrolled++
		} else {
			skip(id, "already_enrolled")
		}
	}
	b, err := pyjson.Dumps(pyjson.NewObj("enrolled", enrolled, "skipped", skipped,
		"total_lead_ids", int64(len(ids))), 0)
	return string(b), err
}

func sendEmail(ctx context.Context, tx pgx.Tx, d Deps, c Ctx, a Args) (string, error) {
	const tool = "send_email"
	if d.Outreach == nil {
		return unavailableJSON(tool), nil
	}
	leadID, ok, err := a.Int("lead_id")
	if err != nil {
		return errJSON(err.Error()), nil
	}
	if !ok {
		return errJSON("lead_id is required"), nil
	}
	seqID, _, err := a.String("sequence_id")
	if err != nil {
		return errJSON(err.Error()), nil
	}
	step, _, err := a.Int("step_number")
	if err != nil {
		return errJSON(err.Error()), nil
	}
	if step < 0 || step > 1<<31-1 {
		return errJSON("step_number must be an integer"), nil
	}
	subject, _, err := a.String("subject")
	if err != nil {
		return errJSON(err.Error()), nil
	}
	if utf8.RuneCountInString(subject) > maxSubjectChars {
		return errJSON("subject must be at most 998 characters"), nil
	}
	body, _, err := a.String("body_html")
	if err != nil {
		return errJSON(err.Error()), nil
	}
	if len(body) > maxBodyHTMLBytes {
		return errJSON("body_html must be at most 204800 bytes"), nil
	}
	idem, _, err := a.String("idempotency_key")
	if err != nil {
		return errJSON(err.Error()), nil
	}

	// Scoped lookup: a lead in another workspace is a miss, so it has no email
	// and nothing is ever queued for it.
	lead, err := d.LeadPort().ByID(ctx, tx, leadID)
	if err != nil {
		return outreachErr(tool, err)
	}
	data := leadSendData(lead)
	if e, _ := data["email"].(string); strings.TrimSpace(e) == "" {
		return sendPayload(SendResult{Status: "skipped", Summary: "lead has no email", SkipReason: "no_email"})
	}
	res, err := d.OutreachPort().QueueSend(ctx, tx, c.WorkspaceID, QueueSendRequest{
		SequenceID: seqID, StepNumber: int(step), Subject: subject, BodyHTML: body,
		LeadID: leadID, LeadData: data, IdempotencyKey: idem,
	})
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return unavailableJSON(tool), nil
		}
		return "", err
	}
	return sendPayload(res)
}

func leadSendData(l *Lead) map[string]any {
	out := map[string]any{}
	if l == nil {
		return out
	}
	vals := map[string]*string{"email": l.Email, "contact_person": l.ContactPerson,
		"contact_title": l.ContactTitle, "city": l.City, "website": l.Website, "phone": l.Phone}
	for _, f := range sendLeadFields {
		if f == "company" {
			out[f] = l.Company
		} else if v := vals[f]; v != nil {
			out[f] = *v
		}
	}
	return out
}

func sendPayload(r SendResult) (string, error) {
	o := pyjson.NewObj("status", r.Status, "summary", r.Summary)
	if r.SkipReason != "" {
		o.Set("skip_reason", r.SkipReason)
	}
	if r.Error != "" {
		o.Set("error", r.Error)
	}
	b, err := pyjson.Dumps(o, 0)
	return string(b), err
}
