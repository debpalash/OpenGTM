package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// wireFullRun applies GO_EXECUTOR_FULL_RUN (plan 15 WP-WBEXEC) to the run_workbook executor:
// conditions, email verification, per-fact provenance and output columns run here instead of
// being handed back. Output clients resolve credentials through local (nil: environment only);
// the verify cascade is the one the API's verify-email route uses. A malformed flag is fatal.
func wireFullRun(d *workbook.Deps, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, local *secrets.Resolver) error {
	wx, err := config.LoadWbExec(os.Getenv)
	if err != nil {
		return err
	}
	if !wx.FullRun {
		return nil
	}
	d.FullRun, d.Provenance = true, wx.ProvenanceTracking
	client := d.Client
	if client == nil {
		client = egress.NewClientForEnv(cfg.AppEnv, cfg.EgressAllowHosts)
	}
	d.Output = workbook.NewLiveOutputPorts(pool, local, client, log)
	vc := emailverify.LiveConfig{Pool: pool, AppEnv: cfg.AppEnv, EgressAllow: cfg.EgressAllowHosts,
		HTTP: egress.NewOperatorClient(cfg.EgressAllowHosts), SMTPDisabled: os.Getenv("LEADGEN_SMTP_VERIFY") == "0",
		HeloHost: os.Getenv("LEADGEN_SMTP_HELO"), MailFrom: os.Getenv("LEADGEN_SMTP_MAIL_FROM"),
		Keys: func(ctx context.Context, ws, env string) (string, error) {
			if local != nil {
				v, _, err := local.Get(ctx, ws, env)
				return v, err
			}
			if d.Creds != nil {
				return d.Creds.Get(ctx, ws, env)
			}
			return "", nil
		},
		// Reacher URL/timeout are operator settings, REACHER_ENABLED may be per workspace.
		ReacherSettings: func(ctx context.Context, ws string) map[string]any {
			out := map[string]any{}
			if local == nil {
				return out
			}
			for _, k := range []string{"REACHER_URL", "REACHER_TIMEOUT"} {
				v, _, err := local.Get(ctx, "", k)
				if err != nil {
					return map[string]any{}
				}
				if v != "" {
					out[k] = v
				}
			}
			if v, _, err := local.Get(ctx, ws, "REACHER_ENABLED"); err == nil && v != "" {
				out["REACHER_ENABLED"] = v
			}
			return out
		}}
	d.Verify = workbook.LiveVerifier(vc)
	log.Info("run_workbook full run on (GO_EXECUTOR_FULL_RUN)", "provenance", wx.ProvenanceTracking)
	return nil
}
