package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadswrite"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// NewProductionDeps wires the production ports over pool (a runtime, non-superuser
// role). Outreach and Automation stay nil until plans 08 (automations) and 09 (outreach) provide
// adapters: their tools answer "not available in this build" (D9).
func NewProductionDeps(cfg Config, pool *pgxpool.Pool, py ProviderCaller, verify emailverify.Deps) Deps {
	return Deps{
		InWorkspace: func(ctx context.Context, ws string, fn func(pgx.Tx) error) error {
			return tenancy.InWorkspace(ctx, pool, ws, fn)
		},
		IdentityTx: func(ctx context.Context, fn func(pgx.Tx) error) error {
			return pgx.BeginFunc(ctx, pool, fn)
		},
		Cfg:       cfg,
		Leads:     PGLeadPort{},
		Enrich:    &PyEnrichPort{Verify: verify, Worker: py},
		Workbooks: PGWorkbookPort{},
	}
}

// PGLeadPort is the production LeadPort: reads through plan 05 (leadsread),
// writes through plan 04 (leadswrite) and the Python upsert_lead overwrite. Every
// method runs inside the caller's tenancy.InWorkspace transaction.
type PGLeadPort struct{}

func (PGLeadPort) List(ctx context.Context, tx pgx.Tx, p ListParams) ([]Lead, error) {
	return leadsread.ListLeads(ctx, tx, p)
}

func (PGLeadPort) ByID(ctx context.Context, tx pgx.Tx, id int64) (*Lead, error) {
	ls, err := leadsread.LeadsByIDs(ctx, tx, []int64{id})
	if err != nil || len(ls) == 0 {
		return nil, err
	}
	return &ls[0], nil
}

func (PGLeadPort) Stats(ctx context.Context, tx pgx.Tx) (StatsResult, error) {
	return leadsread.Stats(ctx, tx)
}

// upsertColumns are the overwritable columns of Python's upsert_lead: every
// leads column except id, workspace_id, created_at (kept) and the (company, city)
// key. Unset ones take the Lead dataclass defaults, as Python's full overwrite.
var upsertColumns = func() []string {
	set := map[string]bool{"last_enriched_at": true, "company_size_basis": true, "field_provenance": true,
		"technographics": true, "source_url": true, "collection_job_id": true}
	for c := range leadswrite.MutableColumns {
		set[c] = true
	}
	delete(set, "company")
	delete(set, "city")
	cols := make([]string, 0, len(set))
	for c := range set {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	return cols
}()

func leadDefault(col string) any {
	switch col {
	case "score", "enrichment_attempts", "employee_count_exact":
		return int32(0)
	case "score_tier":
		return "unqualified"
	case "status":
		return "new"
	}
	return ""
}

func isoNowUTC() string {
	t := time.Now().UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

// Upsert is upsert_lead: insert, or on a (workspace, company, city) hit
// overwrite every column but created_at with the payload (defaults for fields
// the caller did not send). The workspace is the transaction's own.
func (PGLeadPort) Upsert(ctx context.Context, tx pgx.Tx, f LeadFields) (int64, error) {
	company, _ := f["company"].(string)
	city, _ := f["city"].(string)
	now := isoNowUTC()
	cols := append([]string{"workspace_id", "company", "city", "created_at", "updated_at"}, upsertColumns...)
	args := []any{company, city, now, now}
	ph := []string{"current_setting('app.workspace_id', true)", "$1", "$2", "$3", "$4"}
	sets := []string{"updated_at = EXCLUDED.updated_at"}
	for _, c := range upsertColumns {
		v := leadDefault(c)
		if s, ok := f[c].(string); ok {
			if _, isText := v.(string); isText {
				v = s
			}
		}
		args = append(args, v)
		ph = append(ph, fmt.Sprintf("$%d", len(args)))
		sets = append(sets, c+" = EXCLUDED."+c)
	}
	// Column names come from the fixed list above, never from the caller.
	q := "INSERT INTO leads (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(ph, ", ") + ") " +
		"ON CONFLICT (workspace_id, company, city) DO UPDATE SET " + strings.Join(sets, ", ") + " RETURNING id"
	var id int32
	if err := tx.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadport: upsert: %w", err)
	}
	return int64(id), nil
}

func (PGLeadPort) UpdateFields(ctx context.Context, tx pgx.Tx, id int64, f LeadFields) error {
	_, err := leadswrite.UpdateFields(ctx, tx, id, f)
	return err
}

func (PGLeadPort) SetStatus(ctx context.Context, tx pgx.Tx, id int64, status string) error {
	_, err := leadswrite.UpdateStatus(ctx, tx, id, status)
	return err
}

func (PGLeadPort) Delete(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := leadswrite.Delete(ctx, tx, id)
	return err
}

// PGWorkbookPort creates and deletes workbooks inside the pipeline transaction
// (wbcrud.Service.Create opens its own transaction, which would break the
// write pipeline's rollback and idempotency atomicity). source_config is {} as
// in Python's MCP create_workbook.
type PGWorkbookPort struct{}

func (PGWorkbookPort) Create(ctx context.Context, tx pgx.Tx, ws, name, description string, columns []Column) (string, error) {
	cols, err := wbcrud.MarshalNoEscape(columns)
	if err != nil {
		return "", err
	}
	row, err := gen.New(tx).InsertWorkbook(ctx, gen.InsertWorkbookParams{
		ID: wbcrud.NewID(), Name: name, Description: description, WorkspaceID: ws, SourceType: "empty",
		SourceConfig: []byte("{}"), FilterCriteria: []byte("{}"), ColumnsConfig: cols,
	})
	if err != nil {
		return "", err
	}
	return row.ID, nil
}

func (PGWorkbookPort) Delete(ctx context.Context, tx pgx.Tx, ws, id string) (bool, error) {
	n, err := gen.New(tx).DeleteWorkbook(ctx, gen.DeleteWorkbookParams{WorkbookID: id, WorkspaceID: ws})
	return n > 0, err
}

// ProviderCaller runs provider calls: the rpclocal caller (the in-process providers).
type ProviderCaller = provcall.Caller

// PyEnrichPort is the production EnrichPort: the email cascade and the remaining
// cost-bearing providers run in process (the rpclocal provider caller). The name
// is historical (the provider calls once went to the Python worker).
type PyEnrichPort struct {
	Verify emailverify.Deps
	Worker ProviderCaller
}

func (p *PyEnrichPort) VerifyEmail(ctx context.Context, ws, email string) (VerifyResult, error) {
	r, err := emailverify.Cascade(ctx, p.Verify, ws, email)
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{Status: r.Status, Deliverable: r.Deliverable(), Confidence: r.Confidence, Source: r.Source, Detail: r.Detail}, nil
}

// Py runs one provider through the provider caller and returns its `output`
// object. provcall.ErrUnavailable, ErrUnknownProvider and ErrUnauthorized pass
// through unchanged.
func (p *PyEnrichPort) Py(ctx context.Context, ws, provider string, in any) (json.RawMessage, error) {
	input, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	if p.Worker == nil {
		return nil, provcall.ErrUnavailable
	}
	res, err := p.Worker.Call(ctx, provcall.Call{
		CallID: "mcp-" + hex.EncodeToString(b[:]), Provider: provider, WorkspaceID: ws, Input: input, Timeout: 120 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if res.Err != nil {
		return nil, res.Err
	}
	if res.Status != "ok" {
		return nil, errors.New("provider call " + res.Status)
	}
	return res.Output, nil
}
