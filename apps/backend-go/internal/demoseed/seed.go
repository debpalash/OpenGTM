package demoseed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// ErrNoWorkspace means first-run setup has not created the workspace the demo attaches to yet.
var ErrNoWorkspace = errors.New("demoseed: no workspace yet; complete first-run setup in the UI, then run the seed again")

// RunTarget is what a Runner needs to start the first run.
type RunTarget struct {
	WorkspaceID   string
	OwnerUsername string
	WorkbookID    string
	ColumnIDs     []string
}

// Runner starts the first enrichment run. The production Runner is APIRunner: the run goes through
// POST /api/workbooks/{id}/run, so planning, billing and executor routing are the API's, not a copy.
type Runner interface {
	StartRun(ctx context.Context, t RunTarget) error
}

// Options selects the workspace and the run behaviour.
type Options struct {
	// WorkspaceID, or WorkspaceSlug (default "main", the workspace first-run setup creates).
	WorkspaceID, WorkspaceSlug string
	// Force starts a run even when the workbook already existed (SEED_FORCE_RUN=1); a run already
	// queued or running still blocks it.
	Force bool
	// Runner starts the first run; nil seeds only.
	Runner Runner
}

// Result reports what a pass did. RunErr is a run that could not start; the seed itself still succeeded
// and the next pass retries while the workbook is a draft with no run queued.
type Result struct {
	WorkspaceID string
	Created     bool
	RunStarted  bool
	RunErr      error
}

// Seed creates the demo workbook and its rows in the workspace (once) and starts the first run.
// Idempotent: a rerun adds nothing, and starts a run only when none is queued and the workbook has not
// run yet (or Force).
func Seed(ctx context.Context, pool *pgxpool.Pool, o Options) (Result, error) {
	ws, owner, err := findWorkspace(ctx, pool, o)
	if err != nil {
		return Result{}, err
	}
	res := Result{WorkspaceID: ws}
	var status string
	var queued bool
	err = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		// one seeder at a time per workspace
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "opengtm-demoseed:"+ws); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
		    filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd, refresh_policy)
		  VALUES ($1, $2, $3, 'draft', $4, 'empty', '{}', '{}', $5::json, $6, 0, false, 0, 0, '{}')
		  ON CONFLICT (id) DO NOTHING`, WorkbookID, WorkbookName, WorkbookDescription, ws, string(ColumnsJSON()), len(companies))
		if err != nil {
			return fmt.Errorf("demoseed: insert workbook: %w", err)
		}
		res.Created = tag.RowsAffected() == 1
		if res.Created {
			b := &pgx.Batch{}
			for i, c := range companies {
				data, _ := json.Marshal(map[string]string{"company": c.Name, "website": c.Domain})
				b.Queue(`INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments) VALUES ($1, $2, $3, $4::json, '{}')`,
					WorkbookID, ws, i, string(data))
			}
			br := tx.SendBatch(ctx, b)
			for range companies {
				if _, err := br.Exec(); err != nil {
					_ = br.Close()
					return fmt.Errorf("demoseed: insert rows: %w", err)
				}
			}
			if err := br.Close(); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(status, '') FROM workbooks WHERE id = $1 AND workspace_id = $2`, WorkbookID, ws).Scan(&status); err != nil {
			return fmt.Errorf("demoseed: workbook %s exists in another workspace: %w", WorkbookID, err)
		}
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE type = 'run_workbook' AND status IN ('pending', 'processing')
		    AND payload::jsonb ->> 'workbook_id' = $1)`, WorkbookID).Scan(&queued)
	})
	if err != nil {
		return res, err
	}
	if o.Runner == nil || queued || !(res.Created || o.Force || status == "draft") {
		return res, nil
	}
	if owner == "" {
		res.RunErr = errors.New("demoseed: the workspace has no owner to start the run as")
		return res, nil
	}
	if err := o.Runner.StartRun(ctx, RunTarget{WorkspaceID: ws, OwnerUsername: owner, WorkbookID: WorkbookID, ColumnIDs: EnrichmentColumnIDs()}); err != nil {
		res.RunErr = err
		return res, nil
	}
	res.RunStarted = true
	return res, nil
}

// findWorkspace resolves the workspace and its owner's username ("" when it has none).
func findWorkspace(ctx context.Context, pool *pgxpool.Pool, o Options) (id, owner string, err error) {
	var ownerName *string
	q := `SELECT w.id, u.username FROM workspaces w LEFT JOIN users u ON u.id = w.owner_id `
	if o.WorkspaceID != "" {
		err = pool.QueryRow(ctx, q+`WHERE w.id = $1`, o.WorkspaceID).Scan(&id, &ownerName)
	} else {
		slug := strings.TrimSpace(o.WorkspaceSlug)
		if slug == "" {
			slug = "main"
		}
		err = pool.QueryRow(ctx, q+`WHERE w.slug = $1`, slug).Scan(&id, &ownerName)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNoWorkspace
	}
	if err != nil {
		return "", "", fmt.Errorf("demoseed: find workspace: %w", err)
	}
	if ownerName != nil {
		owner = *ownerName
	}
	return id, owner, nil
}

// APIRunner starts the run through the API as the workspace owner, with a short-lived access token it signs
// with the instance's SECRET_KEY (the seed runs next to the API, with the same secret).
type APIRunner struct {
	BaseURL string
	Signer  auth.Signer
	Client  *http.Client
}

// StartRun is POST /api/workbooks/{id}/run with the demo's enrichment columns.
func (r APIRunner) StartRun(ctx context.Context, t RunTarget) error {
	tok, err := r.Signer.Access(t.OwnerUsername, []string{"pwd"})
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"column_ids": t.ColumnIDs, "fill_missing": false})
	url := strings.TrimRight(r.BaseURL, "/") + "/api/workbooks/" + t.WorkbookID + "/run"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Workspace-Id", t.WorkspaceID)
	req.Header.Set("Content-Type", "application/json")
	c := r.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("demoseed: start run: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("demoseed: start run: API answered HTTP %d", resp.StatusCode)
	}
	return nil
}
