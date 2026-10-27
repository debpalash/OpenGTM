package sourcewb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Collector runs one leadgen collection job for a query and returns its id once the job
// has finished (JobRunner.submit). The job's stages and leads are read back from
// collection_job_stages and leads. Implemented by internal/collect.
type Collector interface {
	Submit(ctx context.Context, workspaceID, query string) (jobID string, err error)
}

// KeyedCollector is a Collector that can make Submit idempotent: the same key in the same workspace returns
// the collection an earlier attempt already ran instead of starting (and paying for) a new one.
type KeyedCollector interface {
	Collector
	SubmitKeyed(ctx context.Context, workspaceID, query, key string) (jobID string, err error)
}

type jobKeyCtx struct{}

// WithJobKey tags ctx with the idempotency key of the queue job being run (Handle sets it from the claim id).
func WithJobKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, jobKeyCtx{}, key)
}

func jobKey(ctx context.Context) string { k, _ := ctx.Value(jobKeyCtx{}).(string); return k }

// LeadGate is the pre-materialization lead check: lead_validator.validate_lead_light and
// account_discovery.evaluate_account_fit. lead is the Lead dataclass as an ordered object
// (asdict field order). AccountFit returns the fit dict (accepted, canonical_domain,
// fit_reasons, criteria_evidence, evidence_urls, retrieved_at, field_confidence,
// rejection_reasons, evaluated_at). Implemented by internal/collect + internal/leadpeople.
type LeadGate interface {
	ValidateLight(lead *wbcrud.Obj) (ok bool, reason string)
	AccountFit(lead, brief *wbcrud.Obj) (*wbcrud.Obj, error)
}

// PeopleQuery is one CrossLinkedProvider.find_people_by_titles call.
type PeopleQuery struct {
	Company        string
	Titles         []string
	Geo, Seniority string
	MaxPeople      int
	MaxSearches    int // remaining budget for this call
}

// Person is one discovered profile ({name, title, linkedin[, evidence_url]}).
type Person struct {
	Name, Title, LinkedIn, EvidenceURL string
}

// PeopleResult is (people, searches_used) plus the provider's search_failures.
type PeopleResult struct {
	People         []Person
	SearchesUsed   int
	SearchFailures int
}

// PeopleFinder is the keyless LinkedIn people discovery (CrossLinked over the search
// browser op). Implemented by internal/leadpeople / the crosslinked provider.
type PeopleFinder interface {
	FindPeopleByTitles(ctx context.Context, q PeopleQuery) (PeopleResult, error)
}

// CRMFetcher pulls contacts from a CRM (crm.hubspot / crm.salesforce fetch_contacts).
type CRMFetcher interface {
	FetchContacts(ctx context.Context, workspaceID, crmName string, limit int, filter string, fieldMap crm.FieldMap) crm.FetchResult
}

// CRMClients is the production CRMFetcher over the Go CRM clients.
type CRMClients struct {
	HubSpot    *crm.HubSpot
	Salesforce *crm.Salesforce
}

// FetchContacts dispatches to the named client; an unknown name is a failed result.
func (c CRMClients) FetchContacts(ctx context.Context, ws, name string, limit int, filter string, fm crm.FieldMap) crm.FetchResult {
	switch {
	case name == "hubspot" && c.HubSpot != nil:
		return c.HubSpot.FetchContacts(ctx, ws, limit, filter, fm)
	case name == "salesforce" && c.Salesforce != nil:
		return c.Salesforce.FetchContacts(ctx, ws, limit, filter, fm)
	}
	return crm.FetchResult{Error: "unsupported crm '" + name + "'"}
}

// Deps are the engine dependencies. Nil seams make the kinds that need them
// unsupported (ErrUnsupported: the worker fails the job permanently).
type Deps struct {
	Pool      *pgxpool.Pool
	Collector Collector
	Gate      LeadGate
	People    PeopleFinder
	CRM       CRMFetcher
	// Hook fires on_row_added for the inserted rows, inside the write transaction.
	Hook automationhook.Hook
	// Progress carries row_added / source_done (nil: none).
	Progress progress.Publisher
	// CRMImportEnabled / PeopleSearchEnabled are CRM_IMPORT_ENABLED and
	// PEOPLE_SEARCH_SOURCE_ENABLED.
	CRMImportEnabled    bool
	PeopleSearchEnabled bool
	// EnqueueRun chains run_workbook after sourcing (payload enrich_after); nil: executor 'python'.
	EnqueueRun func(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string) error
	Log        *slog.Logger
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d Deps) publish(ctx context.Context, wbID string, msg map[string]any) {
	if d.Progress != nil {
		d.Progress.Publish(ctx, wbID, msg)
	}
}

// ErrUnsupported: this Go worker lacks the seam a source kind needs (collector, lead gate,
// people finder or CRM clients); nothing was written and the job belongs to Python.
var ErrUnsupported = errors.New("sourcewb: source kind not supported by this worker")

// Kind names of a source column.
const (
	KindCollection   = "collection"
	KindPeopleSearch = PersonSource
	KindCRMImport    = "crm_import"
)

// sourceColumn is the resolved source column of a run.
type sourceColumn struct {
	col  wbcrud.Column
	kind string
	crm  map[string]any // {**col, **col["source"]} for crm_import
}

// columnKind is the source-kind dispatch of _materialize_source_impl.
func columnKind(col wbcrud.Column) sourceColumn {
	src, _ := asObj(col.Get("source"))
	k := get(src, "kind")
	if !truthy(k) {
		k = col.Get("kind")
	}
	if strings.ToLower(orStr(k)) == KindCRMImport {
		m := columnMap(col)
		if src != nil {
			for _, key := range src.Keys() {
				m[key], _ = src.Get(key)
			}
		}
		return sourceColumn{col: col, kind: KindCRMImport, crm: m}
	}
	if strip(orStr(col.Get("kind"))) == KindPeopleSearch {
		return sourceColumn{col: col, kind: KindPeopleSearch}
	}
	return sourceColumn{col: col, kind: KindCollection}
}

// supported reports whether this worker has the seams a column needs.
func (d Deps) supported(sc sourceColumn) bool {
	switch sc.kind {
	case KindCRMImport:
		return ready(d.CRM)
	case KindPeopleSearch:
		return ready(d.People)
	}
	return ready(d.Collector) && ready(d.Gate)
}

// ready: the seam is set and, when it can tell, available in this process.
func ready(seam any) bool {
	if seam == nil {
		return false
	}
	if a, ok := seam.(availability); ok {
		return a.Available()
	}
	return true
}

// loadColumn reads the workbook's source column; ok=false with a Python error code
// ("workbook_not_found", "source_column_not_found").
func loadColumn(ctx context.Context, q *gen.Queries, ws, wbID, colID string) (sourceColumn, *wbcrud.Obj, string, error) {
	wb, err := q.SWLoadWorkbook(ctx, gen.SWLoadWorkbookParams{ID: wbID, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return sourceColumn{}, nil, "workbook_not_found", nil
	}
	if err != nil {
		return sourceColumn{}, nil, "", fmt.Errorf("sourcewb: load workbook: %w", err)
	}
	cols, err := wbcrud.ParseColumns(wb.ColumnsConfig)
	if err != nil {
		return sourceColumn{}, nil, "", fmt.Errorf("sourcewb: columns_config: %w", err)
	}
	for _, c := range cols {
		if id, _ := c.Get("id").(string); id == colID {
			if t, _ := c.Get("type").(string); t == "source" {
				sc, _ := wbcrud.ParseObject(nonEmptyJSON(wb.SourceConfig))
				return columnKind(c), sc, "", nil
			}
		}
	}
	return sourceColumn{}, nil, "source_column_not_found", nil
}

func nonEmptyJSON(b []byte) []byte {
	if s := strings.TrimSpace(string(b)); s == "" || s == "null" {
		return []byte("{}")
	}
	return b
}

// Materialize is materialize_source: run a workbook's source column inside the
// workspace and append new rows. Errors of the run itself are data in the returned
// summary ({"error": ...}); a returned error is a database/infrastructure failure
// (the queue retries the job) or ErrUnsupported.
func Materialize(ctx context.Context, d Deps, ws, wbID, colID string) (*wbcrud.Obj, error) {
	res, err := materialize(ctx, d, ws, wbID, colID)
	if err != nil && ctx.Err() != nil && !errors.Is(err, ErrUnsupported) {
		// A cancelled run (user cancel, lease loss, shutdown) must not leave the workbook "running". The
		// terminal state is written on a context that outlives the cancellation; a retry sets running again.
		_ = d.setStatus(context.WithoutCancel(ctx), ws, wbID, StatusCancelled)
	}
	return res, err
}

// StatusCancelled is the workbook status of a run that was cancelled mid-flight.
const StatusCancelled = "cancelled"

func materialize(ctx context.Context, d Deps, ws, wbID, colID string) (*wbcrud.Obj, error) {
	if ws == "" {
		return nil, errors.New("sourcewb: workspace_id is required (empty would yield zero rows under RLS and mask the bug)")
	}
	var sc sourceColumn
	var code string
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) (e error) {
		sc, _, code, e = loadColumn(ctx, gen.New(tx), ws, wbID, colID)
		return e
	})
	if err != nil {
		return nil, err
	}
	if code != "" {
		return obj("error", code), nil
	}
	if !d.supported(sc) {
		return nil, ErrUnsupported
	}
	switch sc.kind {
	case KindCRMImport:
		return d.materializeCRM(ctx, ws, wbID, colID, sc.crm)
	case KindPeopleSearch:
		if err := d.setStatus(ctx, ws, wbID, "running"); err != nil {
			return nil, err
		}
		return d.materializePeople(ctx, ws, wbID, colID, columnMap(sc.col))
	}
	return d.materializeCollection(ctx, ws, wbID, colID, sc.col)
}

// SupportedKind reports whether Materialize can run the column without ErrUnsupported.
func (d Deps) SupportedKind(col wbcrud.Column) bool { return d.supported(columnKind(col)) }

func (d Deps) setStatus(ctx context.Context, ws, wbID, status string) error {
	return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		return gen.New(tx).SWSetStatus(ctx, gen.SWSetStatusParams{Status: status, ID: wbID, WorkspaceID: ws})
	})
}

// runWrites is the shared write phase: fn inserts rows inside one tenant transaction and
// returns the events to publish after commit; the on_row_added hook fires in the same
// transaction. source_done is published whatever happens (Python's finally).
type writeOut struct {
	added, skipped int
	rowIDs         []int64
	events         []map[string]any
}

func (d Deps) runWrites(ctx context.Context, ws, wbID, colID string, found int,
	fn func(tx pgx.Tx, q *gen.Queries, out *writeOut) error) (writeOut, error) {
	var out writeOut
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		out = writeOut{}
		if err := fn(tx, gen.New(tx), &out); err != nil {
			return err
		}
		if len(out.rowIDs) > 0 && d.Hook != nil {
			if _, err := d.Hook.RowsAdded(ctx, tx, ws, wbID, out.rowIDs); err != nil {
				d.log().Warn("on_row_added emit (source) failed", "err", err)
			}
		}
		return nil
	})
	if err != nil {
		// The rows were rolled back: report nothing added, and free the workbook
		// (Python leaves it "running" until a retry; deviation).
		_ = d.setStatus(context.WithoutCancel(ctx), ws, wbID, "draft")
		d.publish(ctx, wbID, map[string]any{"type": "source_done", "columnId": colID, "found": found, "added": 0, "skipped": 0})
		return writeOut{}, err
	}
	for _, e := range out.events {
		d.publish(ctx, wbID, e)
	}
	d.publish(ctx, wbID, map[string]any{"type": "source_done", "columnId": colID,
		"found": found, "added": out.added, "skipped": out.skipped})
	return out, nil
}

func idsAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// marshalAny is json.Marshal without HTML escaping.
func marshalAny(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
