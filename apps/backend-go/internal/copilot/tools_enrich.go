package copilot

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadswrite"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// ProviderLookup finds an enrichment provider by registry name (providers.Get in production).
type ProviderLookup interface {
	Get(name string) (providers.Provider, bool)
}

// Registry names of the providers enrich_lead runs, and the scrape budget (Python: asyncio.wait_for 30 s).
const (
	enrichScraperName = "website_scraper"
	enrichEmailName   = "ddg_email"
)

var enrichScrapeTimeout = 30 * time.Second

func leadHasWebsite(l leadsread.Lead) bool { return notPlaceholder(sval(l.Website)) }

// enrichProvider returns the named provider when it runs in this process (a gated providers/scrape provider
// only when GO_LOCAL_PROVIDERS lists it).
func (t *Turn) enrichProvider(name string) (providers.Provider, bool) {
	if t.Engine.Providers == nil {
		return nil, false
	}
	p, ok := t.Engine.Providers.Get(name)
	if !ok || !providers.InProcess(p) {
		return nil, false
	}
	return p, true
}

func enrichInput(l leadsread.Lead) providers.Input {
	return providers.Input{Field: "email", Lead: map[string]any{
		"id": l.ID, "company": l.Company, "website": sval(l.Website), "email": sval(l.Email), "phone": sval(l.Phone),
		"city": sval(l.City), "contact_person": sval(l.ContactPerson), "linkedin_url": sval(l.LinkedinURL),
	}}
}

func outString(o *providers.Obj, key string) string {
	if o == nil {
		return ""
	}
	v, _ := o.Get(key)
	s, _ := v.(string)
	return s
}

// enrichLead ports tools.py enrich_lead: when the lead has a website and lacks an email or a phone, its site
// is scanned (website_scraper: home, /contact, /contact-us, /about) and the email, phone, LinkedIn URL and
// description it finds fill the empty fields. Deviations: a lead still without an email afterwards gets one
// ddg_email search (the tool's "contact discovery"); a scrape failure is reported as a fixed website_error
// entry (Python echoed the exception) and alone writes nothing (Python then bumped last_enriched_at).
func (t *Turn) enrichLead(ctx context.Context, a map[string]any) (string, bool, error) {
	raw, present := a["lead_id"]
	if !present {
		return keyErr("enrich_lead", "lead_id"), true, nil
	}
	scraper, hasScraper := t.enrichProvider(enrichScraperName)
	finder, hasFinder := t.enrichProvider(enrichEmailName)
	if !hasScraper && !hasFinder {
		return "", false, nil
	}
	id, ok := asInt64(raw)
	if !ok {
		return errLeadNotFound, true, nil
	}
	var lead *leadsread.Lead
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		lead, err = leadByID(ctx, tx, id)
		return err
	})
	if err != nil {
		return "", false, err
	}
	if lead == nil {
		return errLeadNotFound, true, nil
	}
	l := *lead
	email, phone, linkedin, desc := sval(l.Email), sval(l.Phone), sval(l.LinkedinURL), sval(l.Description)
	fields := []string{}
	update := map[string]any{}
	set := func(name, col, v string, dst *string) {
		*dst = v
		update[col] = v
		fields = append(fields, name)
	}

	if leadHasWebsite(l) && (!leadHasEmail(l) || !leadHasPhone(l)) {
		switch {
		case !hasScraper:
			fields = append(fields, "website_error: website scraper is not available")
		case t.Engine.URLCheck != nil && t.Engine.URLCheck(normalizeSite(*l.Website)) != nil:
			fields = append(fields, "website_error: website URL is not allowed")
		default:
			sctx, cancel := context.WithTimeout(providers.WithWorkspace(ctx, t.WS), enrichScrapeTimeout)
			out, err := scraper.Enrich(sctx, nil, noKey, enrichInput(l))
			cancel()
			switch {
			case ctx.Err() != nil:
				return "", false, ctx.Err()
			case err != nil:
				t.logger().Warn("copilot enrich_lead website scrape failed", "err_type", fmt.Sprintf("%T", err))
				fields = append(fields, "website_error: website scrape failed")
			default:
				if v := outString(out.Extra, "email"); v != "" && !leadHasEmail(l) {
					set("email", "email", v, &email)
				}
				if v := outString(out.Extra, "phone"); v != "" && !leadHasPhone(l) {
					set("phone", "phone", v, &phone)
				}
				if v := outString(out.Extra, "linkedin_url"); v != "" && !leadHasLinkedin(l) {
					set("linkedin", "linkedin_url", v, &linkedin)
				}
				if v := outString(out.Extra, "description"); v != "" && desc == "" {
					set("description", "description", v, &desc)
				}
			}
		}
	}
	if hasFinder && update["email"] == nil && !leadHasEmail(l) && (l.Company != "" || leadHasWebsite(l)) {
		sctx, cancel := context.WithTimeout(providers.WithWorkspace(ctx, t.WS), enrichScrapeTimeout)
		out, err := finder.Enrich(sctx, nil, noKey, enrichInput(l))
		cancel()
		switch {
		case ctx.Err() != nil:
			return "", false, ctx.Err()
		case err != nil:
			t.logger().Warn("copilot enrich_lead email search failed", "err_type", fmt.Sprintf("%T", err))
		default:
			if v, _ := out.Value.(string); out.Error == "" && v != "" {
				set("email", "email", v, &email)
			}
		}
	}

	if len(update) > 0 {
		update["last_enriched_at"] = time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00")
		err := t.inTx(ctx, func(tx pgx.Tx) error {
			_, err := leadswrite.UpdateFields(ctx, tx, l.ID, update)
			return err
		})
		if err != nil {
			return "", false, err
		}
	}
	out, err := pyJSON(struct {
		OK             bool     `json:"ok"`
		LeadID         int64    `json:"lead_id"`
		Company        string   `json:"company"`
		EnrichedFields []string `json:"enriched_fields"`
		Email          *string  `json:"email"`
		Phone          *string  `json:"phone"`
		Linkedin       *string  `json:"linkedin"`
	}{true, l.ID, l.Company, fields, keep(l.Email, email), keep(l.Phone, phone), keep(l.LinkedinURL, linkedin)})
	return out, err == nil, err
}

// keep is the field after enrichment: the new value, or the original (NULL stays null, like lead.email = None).
func keep(orig *string, v string) *string {
	if orig == nil && v == "" {
		return nil
	}
	return &v
}

// normalizeSite is Python's `url if url.startswith("http") else f"https://{url}"`.
func normalizeSite(u string) string {
	if len(u) >= 4 && u[:4] == "http" {
		return u
	}
	return "https://" + u
}

func noKey() (string, error) { return "", nil }
