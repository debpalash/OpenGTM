package pluginrun

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/authz"
	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/progress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// API serves plugin catalog and run endpoints for the caller's workspace.
type API struct {
	pool    *pgxpool.Pool
	catalog *Catalog
	authz   *authz.Client
}

// NewAPI builds the HTTP handlers.
func NewAPI(pool *pgxpool.Pool, catalog *Catalog, az *authz.Client) *API {
	return &API{pool: pool, catalog: catalog, authz: az}
}

// Mount registers the routes. Every route requires workspace authorization;
// creating and cancelling runs also requires an editor (or higher) role.
func (a *API) Mount(mux *http.ServeMux) {
	auth := func(h http.HandlerFunc) http.Handler {
		if a.authz == nil || a.pool == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authz.WriteError(w, authz.ErrUnavailable)
			})
		}
		return a.authz.Middleware(h)
	}
	mux.Handle("GET /api/v2/plugins", auth(a.listPlugins))
	mux.Handle("GET /api/v2/plugin-runs", auth(a.listRuns))
	mux.Handle("POST /api/v2/plugin-runs", auth(a.createRun))
	mux.Handle("GET /api/v2/plugin-runs/{id}", auth(a.getRun))
	mux.Handle("GET /api/v2/plugin-runs/{id}/results", auth(a.listResults))
	mux.Handle("POST /api/v2/plugin-runs/{id}/cancel", auth(a.cancelRun))
}

type pluginJSON struct {
	Name        string         `json:"name"`
	DisplayName string         `json:"display_name"`
	Kind        string         `json:"kind"`
	Runtime     string         `json:"runtime"`
	Version     string         `json:"version"`
	Author      string         `json:"author"`
	License     string         `json:"license"`
	Description string         `json:"description"`
	Tags        []string       `json:"tags"`
	Source      string         `json:"source"`
	Signature   string         `json:"signature"`
	Network     []string       `json:"network"`
	Secrets     []string       `json:"secrets"`
	Browser     bool           `json:"browser"`
	Inputs      map[string]any `json:"inputs"`
	Outputs     any            `json:"outputs"`
	Runnable    bool           `json:"runnable"`
	Unrunnable  string         `json:"unrunnable_reason,omitempty"`
}

func (a *API) listPlugins(w http.ResponseWriter, r *http.Request) {
	out := []pluginJSON{}
	for _, e := range a.catalog.List() {
		p := e.Plugin
		network := p.Capabilities.Network
		if network == nil {
			network = []string{}
		}
		pj := pluginJSON{
			Name: p.Name, DisplayName: p.DisplayName, Kind: p.Kind, Runtime: p.Runtime,
			Version: p.Version, Author: p.Author, License: p.License, Description: p.Description,
			Tags: p.Tags, Source: e.Source, Signature: e.Signature.Status,
			Network: network, Secrets: p.Capabilities.Secrets, Browser: p.Capabilities.Browser,
			Inputs: p.Inputs, Outputs: p.Outputs, Runnable: true,
		}
		if pj.Tags == nil {
			pj.Tags = []string{}
		}
		if pj.Secrets == nil {
			pj.Secrets = []string{}
		}
		if p.Runtime == "process" || (p.Runtime == "declarative" && p.Kind != "provider" && p.Kind != "scraper") {
			pj.Runnable, pj.Unrunnable = false, "runtime "+p.Runtime+" for kind "+p.Kind+" is not supported yet"
		}
		out = append(out, pj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": out, "errors": a.catalogErrors(r.Context())})
}

// catalogErrors are shown to admins only; paths reveal server layout.
func (a *API) catalogErrors(ctx context.Context) []LoadError {
	if authz.RequireRole(ctx, "admin") != nil {
		return []LoadError{}
	}
	if a.catalog.Errors == nil {
		return []LoadError{}
	}
	return a.catalog.Errors
}

// Run is the API representation of a plugin run.
type Run struct {
	ID            string          `json:"id"`
	Plugin        string          `json:"plugin"`
	PluginVersion *string         `json:"plugin_version"`
	Status        string          `json:"status"`
	Inputs        json.RawMessage `json:"inputs"`
	JobID         *int64          `json:"job_id"`
	CreatedBy     *string         `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
	StartedAt     *time.Time      `json:"started_at"`
	CompletedAt   *time.Time      `json:"completed_at"`
	Error         *string         `json:"error"`
	Stats         json.RawMessage `json:"stats"`
}

const runColumns = `id::text, plugin_name, plugin_version, status, inputs, job_id, created_by,
	created_at, started_at, completed_at, error, stats`

func scanRun(row pgx.Row) (Run, error) {
	var run Run
	var jobID *int32
	err := row.Scan(&run.ID, &run.Plugin, &run.PluginVersion, &run.Status, &run.Inputs, &jobID,
		&run.CreatedBy, &run.CreatedAt, &run.StartedAt, &run.CompletedAt, &run.Error, &run.Stats)
	if jobID != nil {
		id := int64(*jobID)
		run.JobID = &id
	}
	return run, err
}

type createRequest struct {
	Plugin string         `json:"plugin"`
	Inputs map[string]any `json:"inputs"`
}

func (a *API) createRun(w http.ResponseWriter, r *http.Request) {
	if err := authz.RequireRole(r.Context(), "admin", "editor"); err != nil {
		authz.WriteError(w, err)
		return
	}
	ws, _ := authz.FromContext(r.Context())
	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeDetail(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}
	entry, ok := a.catalog.Get(req.Plugin)
	if !ok {
		writeDetail(w, http.StatusNotFound, "Plugin not installed")
		return
	}
	if req.Inputs == nil {
		req.Inputs = map[string]any{}
	}
	if err := entry.Plugin.ValidateInputs(req.Inputs); err != nil {
		writeDetail(w, http.StatusUnprocessableEntity, "Invalid inputs: "+err.Error())
		return
	}
	inputs, _ := json.Marshal(req.Inputs)

	var run Run
	err := db.WithTenant(r.Context(), a.pool, ws.WorkspaceID, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(r.Context(), `INSERT INTO plugin_runs
			(workspace_id, plugin_name, plugin_version, inputs, created_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
			ws.WorkspaceID, entry.Plugin.Name, entry.Plugin.Version, inputs, ws.Username,
		).Scan(&id); err != nil {
			return err
		}
		jobID, err := queue.Enqueue(r.Context(), tx, JobType,
			jobPayload{WorkspaceID: ws.WorkspaceID, RunID: id},
			queue.EnqueueOptions{FireKey: "plugin_run:" + id})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE plugin_runs SET job_id = $2 WHERE id = $1`, id, jobID); err != nil {
			return err
		}
		if err := progress.Publish(r.Context(), tx, ws.WorkspaceID, "plugin_run_queued",
			map[string]any{"run_id": id, "plugin": entry.Plugin.Name}); err != nil {
			return err
		}
		run, err = scanRun(tx.QueryRow(r.Context(), `SELECT `+runColumns+` FROM plugin_runs WHERE id = $1`, id))
		return err
	})
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "Could not create plugin run")
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

// listRuns pages through the caller's runs newest first. Paging is keyset
// based on (created_at, id): a cursor names the last row of the previous page,
// so rows created or finished meanwhile never shift, repeat or skip entries
// (OFFSET would). limit rows are returned; one extra row is read only to know
// whether another page exists.
func (a *API) listRuns(w http.ResponseWriter, r *http.Request) {
	ws, _ := authz.FromContext(r.Context())
	limit := intParam(r, "limit", 50, 1, 200)
	var after *runCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeRunCursor(raw)
		if err != nil {
			writeDetail(w, http.StatusBadRequest, "Invalid cursor")
			return
		}
		after = &c
	}
	runs := []Run{}
	err := db.WithTenant(r.Context(), a.pool, ws.WorkspaceID, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if after == nil {
			rows, err = tx.Query(r.Context(), `SELECT `+runColumns+` FROM plugin_runs
				WHERE workspace_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, ws.WorkspaceID, limit+1)
		} else {
			rows, err = tx.Query(r.Context(), `SELECT `+runColumns+` FROM plugin_runs
				WHERE workspace_id = $1 AND (created_at, id) < ($2::timestamptz, $3::uuid)
				ORDER BY created_at DESC, id DESC LIMIT $4`, ws.WorkspaceID, after.CreatedAt, after.ID, limit+1)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				return err
			}
			runs = append(runs, run)
		}
		return rows.Err()
	})
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "Could not list plugin runs")
		return
	}
	var next *string
	if len(runs) > limit {
		runs = runs[:limit]
		c := encodeRunCursor(runs[limit-1])
		next = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "next_cursor": next})
}

// runCursor is the decoded position of a runs page: the last row it returned.
type runCursor struct {
	CreatedAt time.Time
	ID        string
}

// encodeRunCursor makes an opaque cursor. It is not signed: it only carries a
// sort position, and every query is still scoped to the caller's workspace by
// the predicate and by row-level security.
func encodeRunCursor(r Run) string {
	return base64.RawURLEncoding.EncodeToString([]byte(r.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + r.ID))
}

func decodeRunCursor(s string) (runCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return runCursor{}, err
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || !uuidPattern.MatchString(id) {
		return runCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return runCursor{}, err
	}
	return runCursor{CreatedAt: t, ID: id}, nil
}

// uuidPattern matches the canonical text form PostgreSQL returns for run ids.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// getRunIn loads a run in the caller's workspace. A malformed id is simply
// not found, so it never reaches the database as an invalid uuid cast. RLS
// already hides other tenants' rows; the explicit workspace predicate keeps
// the intent visible.
func getRunIn(ctx context.Context, tx pgx.Tx, workspaceID, id string) (Run, error) {
	if !uuidPattern.MatchString(id) {
		return Run{}, pgx.ErrNoRows
	}
	return scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM plugin_runs
		WHERE id = $1::uuid AND workspace_id = $2`, id, workspaceID))
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	ws, _ := authz.FromContext(r.Context())
	var run Run
	err := db.WithTenant(r.Context(), a.pool, ws.WorkspaceID, func(tx pgx.Tx) error {
		var err error
		run, err = getRunIn(r.Context(), tx, ws.WorkspaceID, r.PathValue("id"))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeDetail(w, http.StatusNotFound, "Plugin run not found")
		return
	}
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "Could not load plugin run")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

type resultJSON struct {
	Index    int             `json:"index"`
	Data     json.RawMessage `json:"data"`
	Evidence json.RawMessage `json:"evidence"`
}

func (a *API) listResults(w http.ResponseWriter, r *http.Request) {
	ws, _ := authz.FromContext(r.Context())
	limit := intParam(r, "limit", 100, 1, 1000)
	offset := intParam(r, "offset", 0, 0, 1<<30)
	results := []resultJSON{}
	var total int
	err := db.WithTenant(r.Context(), a.pool, ws.WorkspaceID, func(tx pgx.Tx) error {
		if _, err := getRunIn(r.Context(), tx, ws.WorkspaceID, r.PathValue("id")); err != nil {
			return err
		}
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM plugin_results WHERE run_id = $1::uuid`,
			r.PathValue("id")).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT record_index, data, evidence FROM plugin_results
			WHERE run_id = $1::uuid ORDER BY record_index LIMIT $2 OFFSET $3`, r.PathValue("id"), limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var res resultJSON
			if err := rows.Scan(&res.Index, &res.Data, &res.Evidence); err != nil {
				return err
			}
			results = append(results, res)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeDetail(w, http.StatusNotFound, "Plugin run not found")
		return
	}
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "Could not load results")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "total": total, "offset": offset, "limit": limit})
}

// cancelRun cancels the run and its job together. The worker's lease monitor
// sees the cancelled job and stops the attempt; the queue preserves the
// cancelled status, and result commits recheck the run status under lock.
func (a *API) cancelRun(w http.ResponseWriter, r *http.Request) {
	if err := authz.RequireRole(r.Context(), "admin", "editor"); err != nil {
		authz.WriteError(w, err)
		return
	}
	ws, _ := authz.FromContext(r.Context())
	var run Run
	conflict := false
	err := db.WithTenant(r.Context(), a.pool, ws.WorkspaceID, func(tx pgx.Tx) error {
		var err error
		run, err = getRunIn(r.Context(), tx, ws.WorkspaceID, r.PathValue("id"))
		if err != nil {
			return err
		}
		if run.Status != "pending" && run.Status != "running" {
			conflict = true
			return nil
		}
		if _, err := tx.Exec(r.Context(), `UPDATE plugin_runs
			SET status = 'cancelled', completed_at = now(), error = 'Cancelled by user'
			WHERE id = $1::uuid`, run.ID); err != nil {
			return err
		}
		if run.JobID != nil {
			if _, err := tx.Exec(r.Context(), `UPDATE jobs SET status = 'cancelled', error = 'Cancelled by user'
				WHERE id = $1 AND type = $2 AND workspace_id = $3 AND status IN ('pending', 'processing')`,
				*run.JobID, JobType, ws.WorkspaceID); err != nil {
				return err
			}
		}
		if err := progress.Publish(r.Context(), tx, ws.WorkspaceID, "plugin_run_cancelled",
			map[string]any{"run_id": run.ID}); err != nil {
			return err
		}
		run, err = getRunIn(r.Context(), tx, ws.WorkspaceID, run.ID)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeDetail(w, http.StatusNotFound, "Plugin run not found")
	case err != nil:
		writeDetail(w, http.StatusInternalServerError, "Could not cancel plugin run")
	case conflict:
		writeDetail(w, http.StatusConflict, "Only pending or running plugin runs can be cancelled")
	default:
		writeJSON(w, http.StatusOK, run)
	}
}

func intParam(r *http.Request, name string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return min(hi, max(lo, v))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeDetail matches FastAPI's {"detail": ...} error shape.
func writeDetail(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
