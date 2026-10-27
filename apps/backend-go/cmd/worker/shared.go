package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// workerShared are the collaborators every job family shares, built once per process: the secrets envelope and the
// in-process resolver (CREDENTIALS_SOURCE=local is the only source since the plan 15 cutover), the settings
// registry and operator settings, and the LLM settings. Collect, source workbooks and AI columns used to build
// their own copies.
type workerShared struct {
	env    *secrets.Envelope
	local  *secrets.Resolver
	reg    *settings.Registry
	app    *settings.App
	llmCfg config.LLMSettings
}

func newWorkerShared(cfg config.Config, pool *pgxpool.Pool) (*workerShared, error) {
	env, err := secrets.NewEnvelope(secrets.Config{
		Provider: cfg.SecretsProvider, MasterKey: cfg.SecretsMasterKey, SecretKey: cfg.SecretKey, AppEnv: cfg.AppEnv,
		VaultAddr: cfg.VaultAddr, VaultToken: cfg.VaultToken, VaultTokenFile: cfg.VaultTokenFile,
		VaultTransitKey: cfg.VaultTransitKey, VaultNamespace: cfg.VaultNamespace, VaultCACert: cfg.VaultCACert,
	}, nil)
	if err != nil {
		return nil, err
	}
	reg, err := settings.Load()
	if err != nil {
		return nil, err
	}
	llmCfg, err := config.LLMFrom(config.Getenv)
	if err != nil {
		return nil, err
	}
	store := secrets.NewPgStore(pool)
	return &workerShared{
		env: env, local: secrets.NewResolver(env, store, os.Getenv), reg: reg, llmCfg: llmCfg,
		app: &settings.App{Store: store, Env: env, Secret: reg.SecretSet(), Getenv: os.Getenv},
	}, nil
}

// refuseJob is the handler of a job this worker cannot run (a missing browser service, master key or emitter):
// the job fails permanently with why. Before the cutover such rows were handed back to Python.
func refuseJob(jobType, why string) runner {
	return func(context.Context, *queue.Claim) error {
		return &queue.PermanentError{Err: fmt.Errorf("worker: %s cannot run: %s", jobType, why)}
	}
}

// connectorIndex is the worker's signature-verified manifest index, read from the same connector settings as the
// API's run planner, so the sha256 a payload carries matches.
var connectorIndex = credentials.ConnectorIndex(os.LookupEnv, nil)

// localManifests serves the run payload manifests from the connector index (the Python manifest endpoint is gone).
func localManifests(index func() map[string]manifest.Entry) *credentials.LocalManifests {
	return credentials.NewLocalManifests(index, runplan.ManifestJSON)
}

// goBlockPlanner plans the go block of a run_workbook payload that has none (refresh, source workbook and recompute
// enqueues, Python-era rows reassigned at the cutover) over the workbook's current columns, inside its tenant.
func goBlockPlanner(pool *pgxpool.Pool, index func() map[string]manifest.Entry, concurrency int,
	cfg func() runplan.Config) func(ctx context.Context, ws, wb string) (json.RawMessage, error) {
	return func(ctx context.Context, ws, wb string) (json.RawMessage, error) {
		if ws == "" {
			return nil, workbook.NewRefusal("plan_failed: the job has no workspace")
		}
		var cols []wbcrud.Column
		err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			row, err := gen.New(tx).RefreshWorkbook(ctx, wb)
			if errors.Is(err, pgx.ErrNoRows) {
				return workbook.NewRefusal("plan_failed: workbook not found")
			}
			if err != nil {
				return err
			}
			cols, err = wbcrud.ParseColumns(row.ColumnsConfig)
			return err
		})
		if err != nil {
			return nil, err
		}
		block, err := runplan.GoBlockForColumns(cols, index(), nil, concurrency, cfg())
		if err != nil {
			return nil, workbook.NewRefusal("plan_failed: " + err.Error())
		}
		return json.RawMessage(block), nil
	}
}
