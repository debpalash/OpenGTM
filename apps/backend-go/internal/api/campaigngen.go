package api

import (
	"context"
	"math"
	"net/http"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadswrite"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// POST /api/campaigns/generate (routers/campaigns.py generate_outreach): one LLM call per lead (first 10 ids),
// leads that are missing, have no email, fail to load or get no usable answer are skipped; the status is always 200.

const emailSystemPrompt = `You are an expert B2B sales copywriter. Generate personalized cold outreach emails.

Rules:
- Keep subject lines under 50 chars, curiosity-driven
- Keep body under 150 words
- Reference specific company details (name, size, industry)
- Include a clear CTA (call/meeting)
- No generic templates — each email must feel personally crafted
- Be respectful and professional
- Return JSON: {"subject": "...", "body": "..."}`

type campaignRequest struct {
	LeadIDs          []int64
	ValueProposition string
	Tone             string
	ExtraContext     string
}

type generatedEmail struct {
	Company string `json:"company"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func parseCampaignRequest(o *wbcrud.Obj) (campaignRequest, []httpx.FieldError) {
	var errs []httpx.FieldError
	req := campaignRequest{}
	req.LeadIDs = intList(o, "lead_ids", &errs)
	req.ValueProposition = strField(o, "value_proposition", true, "", 0, -1, &errs)
	req.Tone = strField(o, "tone", false, "professional", 0, -1, &errs)
	req.ExtraContext = strField(o, "extra_context", false, "", 0, -1, &errs)
	return req, errs
}

func orStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func runeHead(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

var toneGuide = map[string]string{
	"professional": "Professional and polished",
	"friendly":     "Warm, conversational, and approachable",
	"direct":       "Direct, bold, and to the point",
	"consultative": "Consultative — position as a helpful advisor",
}

// buildEmailPrompt is _build_email_prompt.
func buildEmailPrompt(l *leadsread.Lead, req campaignRequest) string {
	info := "Company: " + l.Company
	if v := orStr(l.City); v != "" {
		info += "\nLocation: " + v
	}
	if v := orStr(l.CompanySize); v != "" {
		info += "\nSize: " + v + " employees"
	}
	if v := orStr(l.Specialization); v != "" {
		info += "\nIndustry: " + v
	}
	if v := orStr(l.Description); v != "" {
		info += "\nAbout: " + runeHead(v, 200)
	}
	if v := orStr(l.ContactPerson); v != "" {
		info += "\nContact: " + v
	}
	if v := orStr(l.GlassdoorRating); v != "" {
		info += "\nGlassdoor: " + v + "/5"
	}
	tone, ok := toneGuide[req.Tone]
	if !ok {
		tone = req.Tone
	}
	extra := ""
	if req.ExtraContext != "" {
		extra = "Additional context: " + req.ExtraContext
	}
	return "Generate a personalized cold outreach email for:\n\n" + info +
		"\n\nMy value proposition: " + req.ValueProposition +
		"\n\nTone: " + tone +
		"\n\n" + extra +
		"\n\nReturn a JSON object with \"subject\" and \"body\" fields."
}

func (h *llmRoutes) campaignGenerate(w http.ResponseWriter, r *http.Request) {
	o, ok := readLLMBody(w, r)
	if !ok {
		return
	}
	req, errs := parseCampaignRequest(o)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws := h.wsID(r)
	ids := req.LeadIDs
	if len(ids) > 10 {
		ids = ids[:10]
	}
	b := h.newLLM(r.Context(), ws)
	emails := []generatedEmail{}
	for _, id := range ids {
		lead, err := h.store.Load(r.Context(), ws, id)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			continue
		}
		if lead == nil || orStr(lead.Email) == "" {
			continue
		}
		res, err := b.Extract.ExtractJSON(r.Context(), buildEmailPrompt(lead, req), emailSystemPrompt, 0)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			continue // Python's client answered {} on failure
		}
		subject, sOK := res.Get("subject")
		body, bOK := res.Get("body")
		ss, sStr := subject.(string)
		bs, bStr := body.(string)
		if !sOK || !bOK || !sStr || !bStr || ss == "" || bs == "" {
			continue // falsy, or not a str (the pydantic model rejects it and Python skips the lead)
		}
		emails = append(emails, generatedEmail{Company: lead.Company, Subject: ss, Body: bs})
	}
	writePy(w, map[string]any{"emails": emails})
}

// leadStore is the lead persistence the LLM routes use (Postgres in production, a fake in unit tests).
type leadStore interface {
	// Load returns the lead (nil when absent or the id names none) in its own short transaction, never held across
	// an LLM call.
	Load(ctx context.Context, ws string, id int64) (*leadsread.Lead, error)
	// Save is update_lead_fields.
	Save(ctx context.Context, ws string, id int64, fields map[string]any) error
}

type pgLeadStore struct{ pool *pgxpool.Pool }

func (s pgLeadStore) Load(ctx context.Context, ws string, id int64) (*leadsread.Lead, error) {
	if id < 1 || id > math.MaxInt32 { // outside int4 names no lead
		return nil, nil
	}
	var lead *leadsread.Lead
	err := tenancy.InWorkspace(ctx, s.pool, ws, func(tx pgx.Tx) error {
		ls, err := leadsread.LeadsByIDs(ctx, tx, []int64{id})
		if err != nil || len(ls) == 0 {
			return err
		}
		lead = &ls[0]
		return nil
	})
	return lead, err
}

func (s pgLeadStore) Save(ctx context.Context, ws string, id int64, fields map[string]any) error {
	return tenancy.InWorkspace(ctx, s.pool, ws, func(tx pgx.Tx) error {
		_, err := leadswrite.UpdateFields(ctx, tx, id, fields)
		return err
	})
}
