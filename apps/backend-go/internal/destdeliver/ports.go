// Package destdeliver delivers audience members to destinations (plan 10). Only the
// provider ports live here until the delivery engine lands.
package destdeliver

import (
	"context"
	"fmt"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Result is the typed form of the Python return map {"success", "summary", "error"}.
type Result struct {
	Success bool
	Summary string
	Error   string
	// ExternalID is the provider-side id of the delivered record (campaign lead id, sheet
	// range, Airtable record id); empty is Python's None.
	ExternalID string
	// Duplicate and Operation carry what Python's campaign and row adders returned beside
	// success ("duplicate", "operation"); the delivery engine derives the summary text from them.
	Duplicate bool
	Operation string
}

// Request payloads. Every request names its workspace (secret resolution and tenant rows are
// per workspace); secrets travel as values for one call and are never logged. Shapes for the
// not-yet-ported connectors are minimal.
type (
	// WebhookRequest posts a payload to a webhook or Slack destination. Slack resolves
	// config.webhook_secret_ref through Ports.Secrets.
	// TODO(plan10 T20): finalize with the delivery engine.
	WebhookRequest struct {
		WorkspaceID     string
		DestinationType string // webhook | slack
		Config          map[string]any
		Payload         map[string]any
		LeadID          int64
		IdempotencyKey  string
	}
	// CampaignRequest adds one lead to an Instantly or Smartlead campaign.
	// TODO(plan10 T20): finalize with the delivery engine.
	CampaignRequest struct {
		WorkspaceID string
		Config      map[string]any
		Payload     map[string]any
		LeadID      int64
	}
	// RowUpsert upserts one row or record (Google Sheets, Airtable).
	// TODO(plan10 T20): finalize with the delivery engine.
	RowUpsert struct {
		WorkspaceID string
		Config      map[string]any
		Payload     map[string]any
		LeadID      int64
	}
)

// CRMPusher mirrors crmpush.Push (HubSpot/Salesforce; field_map travels inside cfg).
// crmpush.Result carries Value, not Summary.
type CRMPusher interface {
	Push(ctx context.Context, ws string, cfg, leadData map[string]any, workbookID string, leadID int64) crmpush.Result
}

// SecretResolver resolves workspace secrets by name (*secrets.Resolver satisfies it).
type SecretResolver interface {
	Get(ctx context.Context, ws, key string) (value, source string, err error)
}

// WorkspaceOnly adapts a resolver to a name getter that answers only workspace-scoped secrets: a global, env or
// default value reads as unset. Destination header_secret_ref / webhook_secret_ref lookups use it so a workspace
// editor cannot point a reference at a deployment-level key.
func WorkspaceOnly(sec SecretResolver) func(ctx context.Context, ws, name string) (string, error) {
	return func(ctx context.Context, ws, name string) (string, error) {
		if sec == nil {
			return "", nil
		}
		v, src, err := sec.Get(ctx, ws, name)
		if err != nil || src != "workspace" {
			return "", err
		}
		return v, nil
	}
}

// Ports the Python integrations (plans 07/08) behind Go seams.
type (
	WebhookSender interface {
		Send(ctx context.Context, r WebhookRequest) (Result, error)
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
	Secrets SecretResolver // webhook_secret_ref, ad platform tokens
	// Outbound is the secret-ref policy re-checked at delivery for Slack (S-M2); the zero value is
	// the default prefixes with no registry deny-list. SlackPrefixes nil means DefaultSlackRefPrefixes.
	Outbound      secrets.OutboundPolicy
	SlackPrefixes []string
	Webhook       WebhookSender
	CRM           CRMPusher
	Instantly     CampaignAdder
	Smartlead     CampaignAdder
	Sheets        SheetsUpserter
	Airtable      AirtableUpserter
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
