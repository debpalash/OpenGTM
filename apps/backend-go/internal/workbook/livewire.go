package workbook

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/airtable"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/instantly"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/sheets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/smartlead"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Production wiring of the GO_EXECUTOR_FULL_RUN collaborators, shared by the worker and the
// single-cell route.

// Output destinations resolve credentials from the column's own workspace only (plan 15 review
// L6): never the operator's global settings or process environment. Otherwise an editor who
// types a base_id, spreadsheet_id or campaign_id could write into any destination the
// operator's token reaches. This is the post-port destination rule (destinations CRUD and the
// automation webhook) and a deliberate deviation from Python, which fell back to global/env.

// workspaceSecrets is an intkit.Secrets that only answers with a workspace-scoped value.
type workspaceSecrets struct{ r *secrets.Resolver }

func (w workspaceSecrets) Get(ctx context.Context, ws, key string) (string, error) {
	v, ok, err := workspaceSecret(ctx, w.r, ws, key)
	if err != nil || !ok {
		return "", err
	}
	return v, nil
}

// workspaceSecret reads key for ws; ok is false unless the value is the workspace's own.
func workspaceSecret(ctx context.Context, r *secrets.Resolver, ws, key string) (string, bool, error) {
	if r == nil || ws == "" {
		return "", false, nil
	}
	v, src, err := r.Get(ctx, ws, key)
	if err != nil {
		return "", false, err
	}
	if v == "" || src != secrets.SourceWorkspace {
		return "", false, nil
	}
	return v, true, nil
}

// crmPort binds crmpush.Push to its CRM clients. The CRM clients walk workspace > global > env
// themselves, so the port first requires the workspace's own credential and fails closed.
type crmPort struct {
	d   crmpush.Deps
	sec *secrets.Resolver
}

// crmKeys are the credentials a CRM type needs (nil: unknown type, Push reports it).
var crmKeys = map[string][]string{
	"hubspot":    {"HUBSPOT_TOKEN"},
	"salesforce": {"SALESFORCE_INSTANCE_URL", "SALESFORCE_ACCESS_TOKEN"},
}

func (c crmPort) Push(ctx context.Context, ws string, cfg, lead map[string]any, wb string, leadID int64) crmpush.Result {
	typ := crmpush.ParseConfig(cfg).Type
	for _, key := range crmKeys[typ] {
		if _, ok, err := workspaceSecret(ctx, c.sec, ws, key); err != nil || !ok {
			return crmpush.Result{Error: typ + " is not connected for this workspace"}
		}
	}
	return crmpush.Push(ctx, c.d, ws, cfg, lead, wb, leadID)
}

// NewLiveOutputPorts returns the destination clients of output columns. httpc must be the
// SSRF-guarded egress client; sec resolves the workspace's own credentials (nil: none resolve).
func NewLiveOutputPorts(pool *pgxpool.Pool, sec *secrets.Resolver, httpc *http.Client, log *slog.Logger) *OutputPorts {
	var s intkit.Secrets = workspaceSecrets{r: sec} // nil resolver: nothing resolves, destinations are not configured
	return &OutputPorts{
		CRM:       crmPort{d: crmpush.Deps{HubSpot: &crm.HubSpot{Secrets: sec}, Salesforce: &crm.Salesforce{Secrets: sec}, Pool: pool, Log: log}, sec: sec},
		Sheets:    sheets.New(httpc, s),
		Airtable:  airtable.New(httpc, s),
		Instantly: instantly.New(httpc, s),
		Smartlead: smartlead.New(httpc, s),
		Outreach:  PGOutreach{Pool: pool},
	}
}

// LiveVerifier is the email verify cascade over emailverify.NewLive.
func LiveVerifier(cfg emailverify.LiveConfig) EmailVerifier {
	d := emailverify.NewLive(cfg)
	return VerifyFunc(func(ctx context.Context, ws, email string) (emailverify.Result, error) {
		return emailverify.Cascade(ctx, d, ws, email)
	})
}
