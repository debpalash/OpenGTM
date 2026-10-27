package workbook

import (
	"context"
	"log/slog"
	"net/http"
	"os"

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

// crmPort binds crmpush.Push to its CRM clients.
type crmPort struct{ d crmpush.Deps }

func (c crmPort) Push(ctx context.Context, ws string, cfg, lead map[string]any, wb string, leadID int64) crmpush.Result {
	return crmpush.Push(ctx, c.d, ws, cfg, lead, wb, leadID)
}

// envSecrets reads credentials from the process environment only (no secret resolver).
type envSecrets struct{}

func (envSecrets) Get(_ context.Context, _, key string) (string, error) { return os.Getenv(key), nil }

// NewLiveOutputPorts returns the destination clients of output columns. httpc must be the
// SSRF-guarded egress client; sec resolves workspace credentials (nil: environment only,
// like the CRM clients without a resolver).
func NewLiveOutputPorts(pool *pgxpool.Pool, sec *secrets.Resolver, httpc *http.Client, log *slog.Logger) *OutputPorts {
	var s intkit.Secrets = envSecrets{}
	if sec != nil {
		s = intkit.ResolverSecrets{R: sec}
	}
	return &OutputPorts{
		CRM:       crmPort{crmpush.Deps{HubSpot: &crm.HubSpot{Secrets: sec}, Salesforce: &crm.Salesforce{Secrets: sec}, Pool: pool, Log: log}},
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
