// Package destdeliver delivers audience members to destinations (plan 10). Only the
// provider ports live here until the delivery engine lands.
package destdeliver

import (
	"context"
	"fmt"
	"strings"
)

// Result is the typed form of the Python return map {"success", "summary", "error"}.
type Result struct {
	Success bool
	Summary string
	Error   string
}

// Request payloads. Secrets travel as values for one call and are never logged.
type (
	// WebhookRequest posts a payload to a webhook or Slack destination.
	WebhookRequest struct {
		DestinationType string // webhook | slack
		Config          map[string]any
		Payload         map[string]any
		IdempotencyKey  string
	}
	// CRMRequest upserts one lead into HubSpot or Salesforce.
	CRMRequest struct {
		DestinationType string // hubspot | salesforce
		Config          map[string]any
		FieldMap        map[string]any
		Payload         map[string]any
	}
	// CampaignRequest adds one lead to an Instantly or Smartlead campaign.
	CampaignRequest struct {
		Config  map[string]any
		Payload map[string]any
	}
	// RowUpsert upserts one row or record (Google Sheets, Airtable).
	RowUpsert struct {
		Config  map[string]any
		Payload map[string]any
	}
)

// Ports the Python integrations (plans 07/08) behind Go seams.
type (
	WebhookSender interface {
		Send(ctx context.Context, r WebhookRequest) (Result, error)
	}
	CRMPusher interface {
		Push(ctx context.Context, r CRMRequest) (Result, error)
	}
	CampaignAdder interface {
		AddLead(ctx context.Context, r CampaignRequest) (Result, error)
	}
	SheetsUpserter interface {
		Upsert(ctx context.Context, r RowUpsert) (Result, error)
	}
	AirtableUpserter interface {
		Upsert(ctx context.Context, r RowUpsert) (Result, error)
	}
)

// Ports are the delivery seams; a nil port means that destination type is unsupported in Go.
type Ports struct {
	Webhook   WebhookSender
	CRM       CRMPusher
	Instantly CampaignAdder
	Smartlead CampaignAdder
	Sheets    SheetsUpserter
	Airtable  AirtableUpserter
}

// Supports reports whether Go can deliver destinationType with these ports. Ads and
// warehouse deliver over the SSRF-guarded egress client and need no port.
func (p Ports) Supports(destinationType string) bool {
	switch strings.ToLower(strings.TrimSpace(destinationType)) {
	case "webhook", "slack":
		return p.Webhook != nil
	case "hubspot", "salesforce":
		return p.CRM != nil
	case "instantly":
		return p.Instantly != nil
	case "smartlead":
		return p.Smartlead != nil
	case "google_sheets":
		return p.Sheets != nil
	case "airtable":
		return p.Airtable != nil
	case "meta_ads", "google_ads", "linkedin_ads", "warehouse_http":
		return true
	}
	return false
}

// ValidateTypes checks the GO_DEST_TYPES list at startup: an unknown type or one without
// its port is a fatal configuration error, never a silent Python fallback.
func (p Ports) ValidateTypes(types []string) error {
	for _, t := range types {
		if !p.Supports(t) {
			return fmt.Errorf("GO_DEST_TYPES: destination type %q is unknown or has no Go delivery port wired", t)
		}
	}
	return nil
}
